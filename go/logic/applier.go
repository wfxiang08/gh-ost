/*
   Copyright 2016 GitHub Inc.
	 See https://github.com/github/gh-ost/blob/master/LICENSE
*/

package logic

import (
	gosql "database/sql"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/github/gh-ost/go/base"
	"github.com/github/gh-ost/go/binlog"
	"github.com/github/gh-ost/go/mysql"
	"github.com/github/gh-ost/go/sql"

	"github.com/fatih/color"
	"github.com/outbrain/golib/log"
	"github.com/outbrain/golib/sqlutils"
)

const (
	atomicCutOverMagicHint = "ghost-cut-over-sentry"
)

type dmlBuildResult struct {
	query     string
	args      []interface{}
	rowsDelta int64
	err       error
}

func newDmlBuildResult(query string, args []interface{}, rowsDelta int64, err error) *dmlBuildResult {
	return &dmlBuildResult{
		query:     query,
		args:      args,
		rowsDelta: rowsDelta,
		err:       err,
	}
}

func newDmlBuildResultError(err error) *dmlBuildResult {
	return &dmlBuildResult{
		err: err,
	}
}

// Applier connects and writes the the applier-server, which is the server where migration
// happens. This is typically the master, but could be a replica when `--test-on-replica` or
// `--execute-on-replica` are given.
// Applier is the one to actually write row data and apply binlog events onto the ghost table.
// It is where the ghost & changelog tables get created. It is where the cut-over phase happens.
type Applier struct {
	connectionConfig  *mysql.ConnectionConfig
	db                *gosql.DB
	migrationContext  *base.MigrationContext
	finishedMigrating int64
}

func NewApplier(migrationContext *base.MigrationContext) *Applier {
	return &Applier{
		connectionConfig:  migrationContext.ApplierConnectionConfig,
		migrationContext:  migrationContext,
		finishedMigrating: 0,
	}
}

func (this *Applier) InitDBConnections() (err error) {

	applierUri := this.connectionConfig.GetDBUri(this.migrationContext.DatabaseName)
	if this.db, _, err = mysql.GetDB(this.migrationContext.Uuid, applierUri); err != nil {
		return err
	}

	this.db.SetMaxOpenConns(100)

	version, err := base.ValidateConnection(this.db, this.connectionConfig, this.migrationContext)
	if err != nil {
		return err
	}

	this.migrationContext.ApplierMySQLVersion = version
	if err := this.validateAndReadTimeZone(); err != nil {
		return err
	}
	if !this.migrationContext.AliyunRDS && !this.migrationContext.GoogleCloudPlatform {
		if impliedKey, err := mysql.GetInstanceKey(this.db); err != nil {
			return err
		} else {
			this.connectionConfig.ImpliedKey = impliedKey
		}
	}
	if err := this.readTableColumns(); err != nil {
		return err
	}
	log.Infof("Applier initiated on %+v, version %+v", this.connectionConfig.ImpliedKey, this.migrationContext.ApplierMySQLVersion)
	return nil
}

// validateAndReadTimeZone potentially reads server time-zone
func (this *Applier) validateAndReadTimeZone() error {
	query := `select @@global.time_zone`
	if err := this.db.QueryRow(query).Scan(&this.migrationContext.ApplierTimeZone); err != nil {
		return err
	}

	log.Infof("will use time_zone='%s' on applier", this.migrationContext.ApplierTimeZone)
	return nil
}

// readTableColumns reads table columns on applier
func (this *Applier) readTableColumns() (err error) {
	log.Infof("Examining table structure on applier")
	this.migrationContext.OriginalTableColumnsOnApplier, _, err = mysql.GetTableColumns(this.db, this.migrationContext.DatabaseName, this.migrationContext.OriginalTableName)
	if err != nil {
		return err
	}
	return nil
}

// showTableStatus returns the output of `show table status like '...'` command
func (this *Applier) showTableStatus(tableName string) (rowMap sqlutils.RowMap) {
	rowMap = nil
	query := fmt.Sprintf(`show /* gh-ost */ table status from %s like '%s'`, sql.EscapeName(this.migrationContext.DatabaseName), tableName)
	sqlutils.QueryRowsMap(this.db, query, func(m sqlutils.RowMap) error {
		rowMap = m
		return nil
	})
	return rowMap
}

// tableExists checks if a given table exists in database
func (this *Applier) tableExists(tableName string) (tableFound bool) {
	m := this.showTableStatus(tableName)
	return (m != nil)
}

// ValidateOrDropExistingTables verifies ghost and changelog tables do not exist,
// or attempts to drop them if instructed to.
func (this *Applier) ValidateOrDropExistingTables() error {
	if this.migrationContext.InitiallyDropGhostTable {
		if err := this.DropGhostTable(); err != nil {
			return err
		}
	}
	if this.tableExists(this.migrationContext.GetGhostTableName()) {
		return fmt.Errorf("Table %s already exists. Panicking. Use --initially-drop-ghost-table to force dropping it, though I really prefer that you drop it or rename it away", sql.EscapeName(this.migrationContext.GetGhostTableName()))
	}
	if this.migrationContext.InitiallyDropOldTable {
		if err := this.DropOldTable(); err != nil {
			return err
		}
	}

	// 验证Drop的结果
	if len(this.migrationContext.GetOldTableName()) > mysql.MaxTableNameLength {
		log.Fatalf("--timestamp-old-table defined, but resulting table name (%s) is too long (only %d characters allowed)", this.migrationContext.GetOldTableName(), mysql.MaxTableNameLength)
	}

	if this.tableExists(this.migrationContext.GetOldTableName()) {
		return fmt.Errorf("Table %s already exists. Panicking. Use --initially-drop-old-table to force dropping it, though I really prefer that you drop it or rename it away", sql.EscapeName(this.migrationContext.GetOldTableName()))
	}

	return nil
}

// CreateGhostTable creates the ghost table on the applier host
func (this *Applier) CreateGhostTable() error {
	// 1. create table like ...., 创建一个schema完全一样的table
	query := fmt.Sprintf(`create /* gh-ost */ table %s.%s like %s.%s`,
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetGhostTableName()),
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.OriginalTableName),
	)

	log.Infof(color.BlueString("Creating ghost table")+" %s.%s",
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetGhostTableName()),
	)

	// 2. 直接执行query
	if _, err := sqlutils.ExecNoPrepare(this.db, query); err != nil {
		return err
	}

	log.Infof("Ghost table created")
	return nil
}

// GetPartitionInfos 获取OriginTable的分区信息
func (this *Applier) GetPartitionInfos() ([]*sql.PartitionInfo, error) {
	query := fmt.Sprintf(`SELECT PARTITION_NAME, TABLE_ROWS FROM INFORMATION_SCHEMA.PARTITIONS  WHERE TABLE_NAME = '%s' AND TABLE_SCHEMA='%s' ORDER BY PARTITION_ORDINAL_POSITION ASC`,
		this.migrationContext.OriginalTableName,
		this.migrationContext.DatabaseName,
	)

	log.Infof("partition info query: %s", color.GreenString(query))
	rows, err := this.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []*sql.PartitionInfo

	for rows.Next() {
		p := &sql.PartitionInfo{}
		err = rows.Scan(&p.PartitionName, &p.TableRows)
		if err != nil {
			log.Info("Scan partitions error: %v", err)
			continue
		} else {
			if p.PartitionName != "" {
				results = append(results, p)
			}
		}
	}
	err = rows.Err()
	if err != nil {
		return nil, err
	}
	return results, nil
}

// AlterGhost applies `alter` statement on ghost table
func (this *Applier) AlterGhost() error {
	query := fmt.Sprintf(`alter /* gh-ost */ table %s.%s %s`,
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetGhostTableName()),
		this.migrationContext.AlterStatement,
	)
	log.Infof(color.BlueString("Altering ghost table")+" %s.%s",
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetGhostTableName()),
	)
	log.Debugf("ALTER statement: %s", query)
	if _, err := sqlutils.ExecNoPrepare(this.db, query); err != nil {
		return err
	}
	log.Infof("Ghost table altered")
	return nil
}

// CreateChangelogTable creates the changelog table on the applier host
func (this *Applier) CreateChangelogTable() error {
	if err := this.DropChangelogTable(); err != nil {
		return err
	}
	query := fmt.Sprintf(`create /* gh-ost */ table %s.%s (
			id bigint auto_increment,
			last_update timestamp not null DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			hint varchar(64) charset ascii not null,
			value varchar(4096) charset ascii not null,
			primary key(id),
			unique key hint_uidx(hint)
		) auto_increment=256
		`,
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetChangelogTableName()),
	)
	log.Infof(color.BlueString("Creating changelog table")+" %s.%s",
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetChangelogTableName()),
	)
	if _, err := sqlutils.ExecNoPrepare(this.db, query); err != nil {
		return err
	}
	log.Infof("Changelog table created")
	return nil
}

// dropTable drops a given table on the applied host
func (this *Applier) dropTable(tableName string) error {
	query := fmt.Sprintf(`drop /* gh-ost */ table if exists %s.%s`,
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(tableName),
	)
	log.Infof(color.BlueString("Droppping table %s.%s"),
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(tableName),
	)
	if _, err := sqlutils.ExecNoPrepare(this.db, query); err != nil {
		return err
	}
	log.Infof("Table dropped")
	return nil
}

// DropChangelogTable drops the changelog table on the applier host
func (this *Applier) DropChangelogTable() error {
	return this.dropTable(this.migrationContext.GetChangelogTableName())
}

// DropOldTable drops the _Old table on the applier host
func (this *Applier) DropOldTable() error {
	return this.dropTable(this.migrationContext.GetOldTableName())
}

// DropGhostTable drops the ghost table on the applier host
func (this *Applier) DropGhostTable() error {
	return this.dropTable(this.migrationContext.GetGhostTableName())
}

// WriteChangelog writes a value to the changelog table.
// It returns the hint as given, for convenience
func (this *Applier) WriteChangelog(hint, value string) (string, error) {
	explicitId := 0
	switch hint {
	case "heartbeat":
		explicitId = 1
	case "state":
		explicitId = 2
	case "throttle":
		explicitId = 3
	}
	query := fmt.Sprintf(`
			insert /* gh-ost */ into %s.%s
				(id, hint, value)
			values
				(NULLIF(?, 0), ?, ?)
			on duplicate key update
				last_update=NOW(),
				value=VALUES(value)
		`,
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetChangelogTableName()),
	)
	_, err := sqlutils.ExecNoPrepare(this.db, query, explicitId, hint, value)
	return hint, err
}

func (this *Applier) WriteAndLogChangelog(hint, value string) (string, error) {
	this.WriteChangelog(hint, value)
	return this.WriteChangelog(fmt.Sprintf("%s at %d", hint, time.Now().UnixNano()), value)
}

func (this *Applier) WriteChangelogState(value string) (string, error) {
	return this.WriteAndLogChangelog("state", value)
}

// InitiateHeartbeat creates a heartbeat cycle, writing to the changelog table.
// This is done asynchronously
func (this *Applier) InitiateHeartbeat() {
	var numSuccessiveFailures int64
	injectHeartbeat := func() error {
		if atomic.LoadInt64(&this.migrationContext.HibernateUntil) > 0 ||
			atomic.LoadInt64(&this.migrationContext.CleanupImminentFlag) > 0 {
			return nil
		}
		if _, err := this.WriteChangelog("heartbeat", time.Now().Format(time.RFC3339Nano)); err != nil {
			numSuccessiveFailures++
			if numSuccessiveFailures > this.migrationContext.MaxRetries() {
				return log.Errore(err)
			}
		} else {
			numSuccessiveFailures = 0
		}
		return nil
	}
	injectHeartbeat()

	//
	// 定时发送心跳数据，用于测量MySQL的同步延迟
	// 如果MySQL的修改多大，那么可能会出现问题
	//
	heartbeatTick := time.Tick(time.Duration(this.migrationContext.HeartbeatIntervalMilliseconds) * time.Millisecond)
	for range heartbeatTick {
		if atomic.LoadInt64(&this.finishedMigrating) > 0 {
			return
		}
		// Generally speaking, we would issue a goroutine, but I'd actually rather
		// have this block the loop rather than spam the master in the event something
		// goes wrong
		if throttle, _, reasonHint := this.migrationContext.IsThrottled(); throttle && (reasonHint == base.UserCommandThrottleReasonHint) {
			continue
		}
		if err := injectHeartbeat(); err != nil {
			return
		}
	}
}

// ExecuteThrottleQuery executes the `--throttle-query` and returns its results.
func (this *Applier) ExecuteThrottleQuery() (int64, error) {
	throttleQuery := this.migrationContext.GetThrottleQuery()

	if throttleQuery == "" {
		return 0, nil
	}
	var result int64
	if err := this.db.QueryRow(throttleQuery).Scan(&result); err != nil {
		return 0, log.Errore(err)
	}
	return result, nil
}

// ReadMigrationMinValues returns the minimum values to be iterated on rowcopy
func (this *Applier) ReadMigrationMinValues(uniqueKey *sql.UniqueKey, partition *sql.PartitionInfo) (string, error) {
	log.Debugf("Reading migration range according to key: %s", uniqueKey.Name)
	query, err := sql.BuildUniqueKeyMinValuesPreparedQuery(this.migrationContext.DatabaseName, this.migrationContext.OriginalTableName, partition, &uniqueKey.Columns)
	if err != nil {
		return "", err
	}
	rows, err := this.db.Query(query)
	if err != nil {
		return "", err
	}
	defer rows.Close() // 必须关闭，否则存在db connection泄露
	for rows.Next() {
		this.migrationContext.MigrationRangeMinValues = sql.NewColumnValues(uniqueKey.Len())
		if err = rows.Scan(this.migrationContext.MigrationRangeMinValues.ValuesPointers...); err != nil {
			return "", err
		}
		return this.migrationContext.MigrationRangeMinValues.String(), nil
	}
	return "", err
}

// ReadMigrationMaxValues returns the maximum values to be iterated on rowcopy
func (this *Applier) ReadMigrationMaxValues(uniqueKey *sql.UniqueKey, partition *sql.PartitionInfo) (string, error) {
	log.Debugf("Reading migration range according to key: %s", uniqueKey.Name)
	query, err := sql.BuildUniqueKeyMaxValuesPreparedQuery(this.migrationContext.DatabaseName, this.migrationContext.OriginalTableName, partition, &uniqueKey.Columns)
	if err != nil {
		return "", err
	}
	rows, err := this.db.Query(query)
	if err != nil {
		return "", err
	}
	defer rows.Close() // 必须关闭

	for rows.Next() {
		this.migrationContext.MigrationRangeMaxValues = sql.NewColumnValues(uniqueKey.Len())
		if err = rows.Scan(this.migrationContext.MigrationRangeMaxValues.ValuesPointers...); err != nil {
			return "", err
		}
		return this.migrationContext.MigrationRangeMaxValues.String(), nil
	}
	return "", err
}

// ReadMigrationRangeValues reads min/max values that will be used for rowcopy
func (this *Applier) ReadMigrationRangeValues(partition *sql.PartitionInfo) error {
	var msgMin string
	var msgMax string
	var err error

	// 读取全局的Range
	if msgMin, err = this.ReadMigrationMinValues(this.migrationContext.UniqueKey, partition); err != nil {
		return err
	}
	if msgMax, err = this.ReadMigrationMaxValues(this.migrationContext.UniqueKey, partition); err != nil {
		return err
	}

	if partition != nil {
		log.Infof("MigrationRange "+color.CyanString("%3s")+" ==> %s ~ %s", partition.PartitionName, msgMin, msgMax)
	} else {
		log.Infof("MigrationRange %s ~ %s", msgMin, msgMax)
	}
	return nil
}

// CalculateNextIterationRangeEndValues reads the next-iteration-range-end unique key values,
// which will be used for copying the next chunk of rows. Ir returns "false" if there is
// no further chunk to work through, i.e. we're past the last chunk and are done with
// iterating the range (and this done with copying row chunks)
func (this *Applier) CalculateNextIterationRangeEndValues(partition *sql.PartitionInfo) (hasFurtherRange bool, err error) {

	// 1. 当前iteration的min value是上次iteration的max value
	this.migrationContext.MigrationIterationRangeMinValues = this.migrationContext.MigrationIterationRangeMaxValues
	//    边界情况
	if this.migrationContext.MigrationIterationRangeMinValues == nil {
		this.migrationContext.MigrationIterationRangeMinValues = this.migrationContext.MigrationRangeMinValues
	}
	for i := 0; i < 2; i++ {
		buildFunc := sql.BuildUniqueKeyRangeEndPreparedQueryViaOffset
		if i == 1 {
			buildFunc = sql.BuildUniqueKeyRangeEndPreparedQueryViaTemptable
		}

		// 给定(MigrationIterationRangeMinValues, MigrationRangeMaxValues, ChunkSize) 得到 iterationRangeMaxValues
		query, explodedArgs, err := buildFunc(
			this.migrationContext.DatabaseName,
			this.migrationContext.OriginalTableName,
			partition,
			&this.migrationContext.UniqueKey.Columns,
			this.migrationContext.MigrationIterationRangeMinValues.AbstractValues(),
			this.migrationContext.MigrationRangeMaxValues.AbstractValues(),
			atomic.LoadInt64(&this.migrationContext.ChunkSize),
			this.migrationContext.GetIteration() == 0,
			fmt.Sprintf("iteration:%d", this.migrationContext.GetIteration()),
		)
		if err != nil {
			return hasFurtherRange, err
		}
		rows, err := this.db.Query(query, explodedArgs...)
		if err != nil {
			return hasFurtherRange, err
		}
		iterationRangeMaxValues := sql.NewColumnValues(this.migrationContext.UniqueKey.Len())
		for rows.Next() {
			if err = rows.Scan(iterationRangeMaxValues.ValuesPointers...); err != nil {
				return hasFurtherRange, err
			}
			hasFurtherRange = true
		}
		if hasFurtherRange {
			this.migrationContext.MigrationIterationRangeMaxValues = iterationRangeMaxValues
			return hasFurtherRange, nil
		}
	}
	if len(this.migrationContext.PartitionInfos) == 0 {
		log.Infof("Iteration complete: no further range to iterate")
	}
	return hasFurtherRange, nil
}

// ApplyIterationInsertQuery issues a chunk-INSERT query on the ghost table. It is where
// data actually gets copied from original table.
func (this *Applier) ApplyIterationInsertQuery(partition *sql.PartitionInfo) (chunkSize int64, rowsAffected int64, duration time.Duration, err error) {
	startTime := time.Now()
	chunkSize = atomic.LoadInt64(&this.migrationContext.ChunkSize)

	// 如何执行区间的Insert呢?
	// 注意控制一下时间: 5s可能是作为一个限制
	query, explodedArgs, err := sql.BuildRangeInsertPreparedQuery(
		this.migrationContext.DatabaseName,
		this.migrationContext.OriginalTableName,
		this.migrationContext.GetGhostTableName(),
		partition,
		this.migrationContext.OriginalFilter,

		this.migrationContext.SharedColumns.Names(),
		this.migrationContext.MappedSharedColumns.Names(),
		this.migrationContext.UniqueKey.Name,
		&this.migrationContext.UniqueKey.Columns,
		this.migrationContext.MigrationIterationRangeMinValues.AbstractValues(),
		this.migrationContext.MigrationIterationRangeMaxValues.AbstractValues(),
		this.migrationContext.GetIteration() == 0,
		this.migrationContext.IsTransactionalTable(),
	)

	if err != nil {
		return chunkSize, rowsAffected, duration, err
	}

	sqlResult, err := func() (gosql.Result, error) {
		tx, err := this.db.Begin()
		if err != nil {
			return nil, err
		}

		// by fei.wang 如果出现错误，则拯救connection
		// 1. 如果调用了tx.Commit, 再调用 tx.Rollback()是会有error返回的，但是"无视结果"
		// 2. 如果tx.Exec 执行过程中，连接断开了，那么Rollback也没有意义了
		// 3. 如果后面的SQL比较简单，基本上不大可能出错，因此可以可以省略Rollback
		defer tx.Rollback()

		// 统一"时区"
		sessionQuery := fmt.Sprintf(`SET
			SESSION time_zone = '%s',
			sql_mode = CONCAT(@@session.sql_mode, ',STRICT_ALL_TABLES')
			`, this.migrationContext.ApplierTimeZone)
		if _, err := tx.Exec(sessionQuery); err != nil {
			return nil, err
		}

		result, err := tx.Exec(query, explodedArgs...)
		if err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return result, nil
	}()

	if err != nil {
		return chunkSize, rowsAffected, duration, err
	}
	rowsAffected, _ = sqlResult.RowsAffected()
	duration = time.Since(startTime)
	log.Debugf(
		"Issued INSERT on range: [%s]..[%s]; iteration: %d; chunk-size: %d",
		this.migrationContext.MigrationIterationRangeMinValues,
		this.migrationContext.MigrationIterationRangeMaxValues,
		this.migrationContext.GetIteration(),
		chunkSize)
	return chunkSize, rowsAffected, duration, nil
}

// RenameTablesRollback renames back both table: original back to ghost,
// _old back to original. This is used by `--test-on-replica`
func (this *Applier) RenameTablesRollback() (renameError error) {
	// Restoring tables to original names.
	// We prefer the single, atomic operation:
	query := fmt.Sprintf(`rename /* gh-ost */ table %s.%s to %s.%s, %s.%s to %s.%s`,
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.OriginalTableName),
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetGhostTableName()),
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetOldTableName()),
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.OriginalTableName),
	)
	log.Infof("Renaming back both tables")
	if _, err := sqlutils.ExecNoPrepare(this.db, query); err == nil {
		return nil
	}
	// But, if for some reason the above was impossible to do, we rename one by one.
	query = fmt.Sprintf(`rename /* gh-ost */ table %s.%s to %s.%s`,
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.OriginalTableName),
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetGhostTableName()),
	)
	log.Infof("Renaming back to ghost table")
	if _, err := sqlutils.ExecNoPrepare(this.db, query); err != nil {
		renameError = err
	}
	query = fmt.Sprintf(`rename /* gh-ost */ table %s.%s to %s.%s`,
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetOldTableName()),
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.OriginalTableName),
	)
	log.Infof("Renaming back to original table")
	if _, err := sqlutils.ExecNoPrepare(this.db, query); err != nil {
		renameError = err
	}
	return log.Errore(renameError)
}

// StopSlaveIOThread is applicable with --test-on-replica; it stops the IO thread, duh.
// We need to keep the SQL thread active so as to complete processing received events,
// and have them written to the binary log, so that we can then read them via streamer.
func (this *Applier) StopSlaveIOThread() error {
	query := `stop /* gh-ost */ slave io_thread`
	log.Infof("Stopping replication IO thread")
	if _, err := sqlutils.ExecNoPrepare(this.db, query); err != nil {
		return err
	}
	log.Infof("Replication IO thread stopped")
	return nil
}

// StartSlaveIOThread is applicable with --test-on-replica
func (this *Applier) StartSlaveIOThread() error {
	query := `start /* gh-ost */ slave io_thread`
	log.Infof("Starting replication IO thread")
	if _, err := sqlutils.ExecNoPrepare(this.db, query); err != nil {
		return err
	}
	log.Infof("Replication IO thread started")
	return nil
}

// StartSlaveSQLThread is applicable with --test-on-replica
func (this *Applier) StopSlaveSQLThread() error {
	query := `stop /* gh-ost */ slave sql_thread`
	log.Infof("Verifying SQL thread is stopped")
	if _, err := sqlutils.ExecNoPrepare(this.db, query); err != nil {
		return err
	}
	log.Infof("SQL thread stopped")
	return nil
}

// StartSlaveSQLThread is applicable with --test-on-replica
func (this *Applier) StartSlaveSQLThread() error {
	query := `start /* gh-ost */ slave sql_thread`
	log.Infof("Verifying SQL thread is running")
	if _, err := sqlutils.ExecNoPrepare(this.db, query); err != nil {
		return err
	}
	log.Infof("SQL thread started")
	return nil
}

// StopReplication is used by `--test-on-replica` and stops replication.
func (this *Applier) StopReplication() error {
	if err := this.StopSlaveIOThread(); err != nil {
		return err
	}
	if err := this.StopSlaveSQLThread(); err != nil {
		return err
	}

	readBinlogCoordinates, executeBinlogCoordinates, err := mysql.GetReplicationBinlogCoordinates(this.db)
	if err != nil {
		return err
	}
	log.Infof("Replication IO thread at %+v. SQL thread is at %+v", *readBinlogCoordinates, *executeBinlogCoordinates)
	return nil
}

// StartReplication is used by `--test-on-replica` on cut-over failure
func (this *Applier) StartReplication() error {
	if err := this.StartSlaveIOThread(); err != nil {
		return err
	}
	if err := this.StartSlaveSQLThread(); err != nil {
		return err
	}
	log.Infof("Replication started")
	return nil
}

// GetSessionLockName returns a name for the special hint session voluntary lock
func (this *Applier) GetSessionLockName(sessionId int64) string {
	return fmt.Sprintf("gh-ost.%d.lock", sessionId)
}

// ExpectUsedLock expects the special hint voluntary lock to exist on given session
func (this *Applier) ExpectUsedLock(sessionId int64) error {
	var result int64
	query := `select is_used_lock(?)`
	lockName := this.GetSessionLockName(sessionId)
	log.Infof("Checking session lock: %s", lockName)
	if err := this.db.QueryRow(query, lockName).Scan(&result); err != nil || result != sessionId {
		return fmt.Errorf("Session lock %s expected to be found but wasn't", lockName)
	}
	return nil
}

// ExpectProcess expects a process to show up in `SHOW PROCESSLIST` that has given characteristics
func (this *Applier) ExpectProcess(sessionId int64, stateHint, infoHint string) error {
	found := false
	query := `
		select id
			from information_schema.processlist
			where
				id != connection_id()
				and ? in (0, id)
				and state like concat('%', ?, '%')
				and info  like concat('%', ?, '%')
	`
	err := sqlutils.QueryRowsMap(this.db, query, func(m sqlutils.RowMap) error {
		found = true
		return nil
	}, sessionId, stateHint, infoHint)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("Cannot find process. Hints: %s, %s", stateHint, infoHint)
	}
	return nil
}

// DropAtomicCutOverSentryTableIfExists checks if the "old" table name
// happens to be a cut-over magic table; if so, it drops it.
func (this *Applier) DropAtomicCutOverSentryTableIfExists() error {
	log.Infof("Looking for magic cut-over table")
	tableName := this.migrationContext.GetOldTableName()
	rowMap := this.showTableStatus(tableName)
	if rowMap == nil {
		// Table does not exist
		return nil
	}
	if rowMap["Comment"].String != atomicCutOverMagicHint {
		return fmt.Errorf("Expected magic comment on %s, did not find it", tableName)
	}
	log.Infof("Dropping magic cut-over table")
	return this.dropTable(tableName)
}

// CreateAtomicCutOverSentryTable
func (this *Applier) CreateAtomicCutOverSentryTable() error {
	if err := this.DropAtomicCutOverSentryTableIfExists(); err != nil {
		return err
	}
	tableName := this.migrationContext.GetOldTableName()

	query := fmt.Sprintf(`create /* gh-ost */ table %s.%s (
			id int auto_increment primary key
		) engine=%s comment='%s'
		`,
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(tableName),
		this.migrationContext.TableEngine,
		atomicCutOverMagicHint,
	)
	log.Infof("Creating magic cut-over table %s.%s",
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(tableName),
	)
	if _, err := sqlutils.ExecNoPrepare(this.db, query); err != nil {
		return err
	}
	log.Infof("Magic cut-over table created")

	return nil
}

// AtomicCutOverMagicLock
func (this *Applier) AtomicCutOverMagicLock(sessionIdChan chan int64, tableLocked chan<- error, okToUnlockTable <-chan bool, tableUnlocked chan<- error) error {
	tx, err := this.db.Begin()
	if err != nil {
		tableLocked <- err
		return err
	}
	defer func() {
		sessionIdChan <- -1
		tableLocked <- fmt.Errorf("Unexpected error in AtomicCutOverMagicLock(), injected to release blocking channel reads")
		tableUnlocked <- fmt.Errorf("Unexpected error in AtomicCutOverMagicLock(), injected to release blocking channel reads")
		tx.Rollback()
	}()

	var sessionId int64
	if err := tx.QueryRow(`select connection_id()`).Scan(&sessionId); err != nil {
		tableLocked <- err
		return err
	}
	sessionIdChan <- sessionId

	lockResult := 0
	query := `select get_lock(?, 0)`
	lockName := this.GetSessionLockName(sessionId)
	log.Infof("Grabbing voluntary lock: %s", lockName)
	if err := tx.QueryRow(query, lockName).Scan(&lockResult); err != nil || lockResult != 1 {
		err := fmt.Errorf("Unable to acquire lock %s", lockName)
		tableLocked <- err
		return err
	}

	tableLockTimeoutSeconds := this.migrationContext.CutOverLockTimeoutSeconds * 2
	log.Infof("Setting LOCK timeout as %d seconds", tableLockTimeoutSeconds)
	query = fmt.Sprintf(`set session lock_wait_timeout:=%d`, tableLockTimeoutSeconds)
	if _, err := tx.Exec(query); err != nil {
		tableLocked <- err
		return err
	}

	// 创建一个place holder table：sentry_table, old_table
	if err := this.CreateAtomicCutOverSentryTable(); err != nil {
		tableLocked <- err
		return err
	}

	// lock tables时该session之前lock的table就被自动释放；注意这个风险
	query = fmt.Sprintf(`lock /* gh-ost */ tables %s.%s write, %s.%s write`,
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.OriginalTableName),
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetOldTableName()),
	)
	log.Infof("Locking %s.%s, %s.%s",
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.OriginalTableName),
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetOldTableName()),
	)
	this.migrationContext.LockTablesStartTime = time.Now()
	if _, err := tx.Exec(query); err != nil {
		tableLocked <- err
		return err
	}
	log.Infof("Tables locked")
	tableLocked <- nil // No error.

	// From this point on, we are committed to UNLOCK TABLES. No matter what happens,
	// the UNLOCK must execute (or, alternatively, this connection dies, which gets the same impact)

	// The cut-over phase will proceed to apply remaining backlog onto ghost table,
	// and issue RENAME. We wait here until told to proceed.
	<-okToUnlockTable
	log.Infof("Will now proceed to drop magic table and unlock tables")

	// The magic table is here because we locked it. And we are the only ones allowed to drop it.
	// And in fact, we will:
	log.Infof("Dropping magic cut-over table")
	query = fmt.Sprintf(`drop /* gh-ost */ table if exists %s.%s`,
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetOldTableName()),
	)
	if _, err := tx.Exec(query); err != nil {
		log.Errore(err)
		// We DO NOT return here because we must `UNLOCK TABLES`!
	}

	// Tables still locked
	log.Infof("Releasing lock from %s.%s, %s.%s",
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.OriginalTableName),
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetOldTableName()),
	)
	query = `unlock tables`
	if _, err := tx.Exec(query); err != nil {
		tableUnlocked <- err
		return log.Errore(err)
	}
	log.Infof("Tables unlocked")
	tableUnlocked <- nil
	return nil
}

// AtomicCutoverRename
func (this *Applier) AtomicCutoverRename(sessionIdChan chan int64, tablesRenamed chan<- error) error {
	tx, err := this.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		tx.Rollback()
		sessionIdChan <- -1
		tablesRenamed <- fmt.Errorf("Unexpected error in AtomicCutoverRename(), injected to release blocking channel reads")
	}()
	var sessionId int64
	if err := tx.QueryRow(`select connection_id()`).Scan(&sessionId); err != nil {
		return err
	}
	sessionIdChan <- sessionId

	log.Infof("Setting RENAME timeout as %d seconds", this.migrationContext.CutOverLockTimeoutSeconds)
	query := fmt.Sprintf(`set session lock_wait_timeout:=%d`, this.migrationContext.CutOverLockTimeoutSeconds)
	if _, err := tx.Exec(query); err != nil {
		return err
	}

	query = fmt.Sprintf(`rename /* gh-ost */ table %s.%s to %s.%s, %s.%s to %s.%s`,
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.OriginalTableName),
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetOldTableName()),
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.GetGhostTableName()),
		sql.EscapeName(this.migrationContext.DatabaseName),
		sql.EscapeName(this.migrationContext.OriginalTableName),
	)

	log.Infof("Issuing and expecting this to block: %s", query)
	if _, err := tx.Exec(query); err != nil {
		tablesRenamed <- err
		return log.Errore(err)
	}
	tablesRenamed <- nil
	log.Infof("Tables renamed")
	return nil
}

func (this *Applier) ShowStatusVariable(variableName string) (result int64, err error) {
	query := fmt.Sprintf(`show global status like '%s'`, variableName)
	if err := this.db.QueryRow(query).Scan(&variableName, &result); err != nil {
		return 0, err
	}
	return result, nil
}

// updateModifiesUniqueKeyColumns checks whether a UPDATE DML event actually
// modifies values of the migration's unique key (the iterated key). This will call
// for special handling.
func (this *Applier) updateModifiesUniqueKeyColumns(dmlEvent *binlog.BinlogDMLEvent) (modifiedColumn string, isModified bool) {
	for _, column := range this.migrationContext.UniqueKey.Columns.Columns() {
		tableOrdinal := this.migrationContext.OriginalTableColumns.Ordinals[column.Name]
		whereColumnValue := dmlEvent.WhereColumnValues.AbstractValues()[tableOrdinal]
		newColumnValue := dmlEvent.NewColumnValues.AbstractValues()[tableOrdinal]
		if newColumnValue != whereColumnValue {
			return column.Name, true
		}
	}
	return "", false
}

// buildDMLEventQuery creates a query to operate on the ghost table, based on an intercepted binlog
// event entry on the original table.
func (this *Applier) buildDMLEventQuery(dmlEvent *binlog.BinlogDMLEvent) (results [](*dmlBuildResult)) {
	switch dmlEvent.DML {
	case binlog.DeleteDML:
		{
			query, uniqueKeyArgs, err := sql.BuildDMLDeleteQuery(dmlEvent.DatabaseName, this.migrationContext.GetGhostTableName(), this.migrationContext.OriginalTableColumns, &this.migrationContext.UniqueKey.Columns, dmlEvent.WhereColumnValues.AbstractValues())
			return append(results, newDmlBuildResult(query, uniqueKeyArgs, -1, err))
		}
	case binlog.InsertDML:
		{
			query, sharedArgs, err := sql.BuildDMLInsertQuery(dmlEvent.DatabaseName, this.migrationContext.GetGhostTableName(), this.migrationContext.OriginalTableColumns, this.migrationContext.SharedColumns, this.migrationContext.MappedSharedColumns, dmlEvent.NewColumnValues.AbstractValues())
			return append(results, newDmlBuildResult(query, sharedArgs, 1, err))
		}
	case binlog.UpdateDML:
		{
			// UpdateDML 如何发现UniqKey本身改变了，则需要演变成为一个Delete + Insert
			if _, isModified := this.updateModifiesUniqueKeyColumns(dmlEvent); isModified {
				dmlEvent.DML = binlog.DeleteDML
				results = append(results, this.buildDMLEventQuery(dmlEvent)...)
				dmlEvent.DML = binlog.InsertDML
				results = append(results, this.buildDMLEventQuery(dmlEvent)...)
				return results
			}
			query, sharedArgs, uniqueKeyArgs, err := sql.BuildDMLUpdateQuery(dmlEvent.DatabaseName, this.migrationContext.GetGhostTableName(), this.migrationContext.OriginalTableColumns, this.migrationContext.SharedColumns, this.migrationContext.MappedSharedColumns, &this.migrationContext.UniqueKey.Columns, dmlEvent.NewColumnValues.AbstractValues(), dmlEvent.WhereColumnValues.AbstractValues())
			args := sqlutils.Args()
			args = append(args, sharedArgs...)
			args = append(args, uniqueKeyArgs...)
			return append(results, newDmlBuildResult(query, args, 0, err))
		}
	}
	return append(results, newDmlBuildResultError(fmt.Errorf("Unknown dml event type: %+v", dmlEvent.DML)))
}

// ApplyDMLEventQueries applies multiple DML queries onto the _ghost_ table
func (this *Applier) ApplyDMLEventQueries(dmlEvents [](*binlog.BinlogDMLEvent)) error {

	var totalDelta int64

	err := func() error {
		tx, err := this.db.Begin()
		if err != nil {
			return err
		}

		rollback := func(err error) error {
			tx.Rollback()
			return err
		}

		// 时区, sql_mode
		// 这个执行时间开销在: 1ms左右，因此后面的dmlEvents最好做批量执行
		// 还好: sql是放在一个transaction内部执行的，这个1ms也省下来了
		//
		sessionQuery := `SET
			SESSION time_zone = '+00:00',
			sql_mode = CONCAT(@@session.sql_mode, ',STRICT_ALL_TABLES')
			`
		if _, err := tx.Exec(sessionQuery); err != nil {
			return rollback(err)
		}
		// 如何处理dmlEvents呢?
		for _, dmlEvent := range dmlEvents {
			for _, buildResult := range this.buildDMLEventQuery(dmlEvent) {
				if buildResult.err != nil {
					return rollback(buildResult.err)
				}
				if _, err := tx.Exec(buildResult.query, buildResult.args...); err != nil {
					err = fmt.Errorf("%s; query=%s; args=%+v", err.Error(), buildResult.query, buildResult.args)
					return rollback(err)
				}
				totalDelta += buildResult.rowsDelta
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return nil
	}()

	if err != nil {
		return log.Errore(err)
	}
	// no error
	atomic.AddInt64(&this.migrationContext.TotalDMLEventsApplied, int64(len(dmlEvents)))
	if this.migrationContext.CountTableRows {
		atomic.AddInt64(&this.migrationContext.RowsDeltaEstimate, totalDelta)
	}
	log.Debugf("ApplyDMLEventQueries() applied %d events in one transaction", len(dmlEvents))
	return nil
}

func (this *Applier) Teardown() {
	log.Debugf("Tearing down...")
	this.db.Close()
	atomic.StoreInt64(&this.finishedMigrating, 1)
}
