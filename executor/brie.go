// Copyright 2020 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/pingcap/br/pkg/glue"
	"github.com/pingcap/br/pkg/storage"
	"github.com/pingcap/br/pkg/task"
	"github.com/pingcap/errors"
	"github.com/pingcap/parser/ast"
	"github.com/pingcap/parser/model"
	"github.com/pingcap/parser/mysql"
	"github.com/pingcap/parser/terror"
	"github.com/pingcap/tidb-lightning/lightning"
	importcfg "github.com/pingcap/tidb-lightning/lightning/config"
	filter "github.com/pingcap/tidb-tools/pkg/table-filter"
	"github.com/pingcap/tidb/config"
	"github.com/pingcap/tidb/ddl"
	"github.com/pingcap/tidb/domain"
	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/sessionctx/stmtctx"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util/chunk"
	"github.com/pingcap/tidb/util/logutil"
	"github.com/pingcap/tidb/util/sqlexec"
	pd "github.com/tikv/pd/client"
	"go.uber.org/zap"
)

const (
	defaultImportID = "tidb_import_"
	cancelTimeout   = 10 * time.Second
)

// brieTaskProgress tracks a task's current progress.
type brieTaskProgress struct {
	// current progress of the task.
	// this field is atomically updated outside of the lock below.
	current int64

	// lock is the mutex protected the two fields below.
	lock sync.Mutex
	// cmd is the name of the step the BRIE task is currently performing.
	cmd string
	// total is the total progress of the task.
	// the percentage of completeness is `(100%) * current / total`.
	total int64
}

// Inc implements glue.Progress
func (p *brieTaskProgress) Inc() {
	atomic.AddInt64(&p.current, 1)
}

// Close implements glue.Progress
func (p *brieTaskProgress) Close() {
	p.lock.Lock()
	atomic.StoreInt64(&p.current, p.total)
	p.lock.Unlock()
}

// GetPercent returns a percentage whose range is [0, 1]
func (p *brieTaskProgress) GetPercent() float64 {
	return float64(p.current) / float64(p.total)
}

type brieTaskInfo struct {
	queueTime   types.Time
	execTime    types.Time
	kind        ast.BRIEKind
	storage     string
	connID      uint64
	backupTS    uint64
	archiveSize uint64
	originSQL   string
}

type brieQueueItem struct {
	info     *brieTaskInfo
	progress *brieTaskProgress
	cancel   func()
}

type brieQueue struct {
	// currently only one BRIE task could run on one TiDB
	workerCh chan struct{}
}

// globalBRIEQueue is the BRIE execution queue. Only one BRIE task can be executed each time.
// TODO: perhaps copy the DDL Job queue so only one task can be executed in the whole cluster.
var globalBRIEQueue = &brieQueue{
	workerCh: make(chan struct{}, 1),
}

// registerTask registers a BRIE task in the queue.
// we should ensure returned *brieQueueItem is not nil
func (bq *brieQueue) registerTask(
	ctx context.Context,
	info *brieTaskInfo,
	se sessionctx.Context,
) (context.Context, uint64, *brieQueueItem, error) {
	s := se.(sqlexec.SQLExecutor)
	taskCtx, taskCancel := context.WithCancel(ctx)
	item := &brieQueueItem{
		info:   info,
		cancel: taskCancel,
		progress: &brieTaskProgress{
			cmd:   "Wait",
			total: 1,
		},
	}

	// use base64 encode to avoid SQL injection
	sql := fmt.Sprintf(`INSERT INTO mysql.brie_tasks (
				kind, origin_sql, queue_time, data_path, conn_id, status, progress, cancel
			) VALUES ('%s', '%s', '%s', '%s', %d, '%s', %f, %d);`,
		info.kind.String(),
		base64.StdEncoding.EncodeToString([]byte(info.originSQL)),
		info.queueTime.String(),
		base64.StdEncoding.EncodeToString([]byte(info.storage)),
		info.connID,
		item.progress.cmd,
		item.progress.GetPercent(),
		0,
	)
	_, err := s.Execute(ctx, sql)
	if err != nil {
		return nil, 0, item, err
	}
	rs, err := s.Execute(ctx, "SELECT LAST_INSERT_ID();")
	if err != nil {
		return nil, 0, item, err
	}
	r := rs[0]
	defer terror.Call(r.Close)
	req := r.NewChunk()
	err = r.Next(ctx, req)
	if err != nil || req.NumRows() != 1 {
		return nil, 0, item, fmt.Errorf("SELECT LAST_INSERT_ID() didn't return one row, err: %v", err)
	}
	taskID := req.GetRow(0).GetUint64(0)
	return taskCtx, taskID, item, nil
}

// acquireTask prepares to execute a BRIE task. Only one BRIE task can be
// executed at a time, and this function blocks until the task is ready.
//
// Returns an object to track the task's progress.
func (bq *brieQueue) acquireTask(taskCtx context.Context, taskID uint64, se sessionctx.Context) (err error) {
	// wait until we are at the front of the queue.
	select {
	case bq.workerCh <- struct{}{}:
		defer func() {
			if err != nil {
				bq.releaseTask()
			}
		}()
		sql := fmt.Sprintf("SELECT cancel FROM mysql.brie_tasks WHERE id = %d;", taskID)
		var rs []sqlexec.RecordSet
		rs, err = se.(sqlexec.SQLExecutor).Execute(taskCtx, sql)
		if err != nil {
			return err
		}
		r := rs[0]
		defer terror.Call(r.Close)
		req := r.NewChunk()
		err = r.Next(taskCtx, req)
		if err != nil {
			return err
		}
		if req.NumRows() == 0 {
			return errors.Errorf("task with ID %d not found", taskID)
		}
		if req.GetRow(0).GetUint64(0) == 1 {
			return errors.Errorf("task with ID %d has been canceled", taskID)
		}
		return nil
	case <-taskCtx.Done():
		return taskCtx.Err()
	}
}

func (bq *brieQueue) releaseTask() {
	<-bq.workerCh
}

func (bq *brieQueue) cancelTask(ctx context.Context, taskID uint64, se sessionctx.Context) {
	// TODO(lance6716): for BR task, delete row to keep compatibility. and rename this function
	sql := fmt.Sprintf("UPDATE mysql.brie_tasks SET cancel = 1 WHERE id = %d;", taskID)
	_, err := se.(sqlexec.SQLExecutor).Execute(ctx, sql)
	if err != nil {
		logutil.Logger(ctx).Error("failed to update BRIE task table to cancel",
			zap.Uint64("taskID", taskID),
			zap.Error(err))
	}
}

func (b *executorBuilder) parseTSString(ts string) (uint64, error) {
	sc := &stmtctx.StatementContext{TimeZone: b.ctx.GetSessionVars().Location()}
	t, err := types.ParseTime(sc, ts, mysql.TypeTimestamp, types.MaxFsp)
	if err != nil {
		return 0, err
	}
	t1, err := t.GoTime(sc.TimeZone)
	if err != nil {
		return 0, err
	}
	return variable.GoTimeToTS(t1), nil
}

func (b *executorBuilder) buildBRIE(s *ast.BRIEStmt, schema *expression.Schema) Executor {
	e := &BRIEExec{
		baseExecutor: newBaseExecutor(b.ctx, schema, 0),
		info: &brieTaskInfo{
			kind:      s.Kind,
			originSQL: s.Text(),
		},
	}

	tidbCfg := config.GetGlobalConfig()
	if tidbCfg.Store != "tikv" {
		b.err = errors.Errorf("%s requires tikv store, not %s", s.Kind, tidbCfg.Store)
		return nil
	}

	var (
		brCfg           task.Config
		importGlobalCfg *importcfg.GlobalConfig
		importCfg       *importcfg.Config
	)
	switch s.Kind {
	case ast.BRIEKindBackup, ast.BRIEKindRestore:
		brCfg = task.Config{
			TLS: task.TLSConfig{
				CA:   tidbCfg.Security.ClusterSSLCA,
				Cert: tidbCfg.Security.ClusterSSLCert,
				Key:  tidbCfg.Security.ClusterSSLKey,
			},
			PD:          strings.Split(tidbCfg.Path, ","),
			Concurrency: 4,
			Checksum:    true,
			SendCreds:   true,
			LogProgress: true,
		}
	case ast.BRIEKindImport:
		importGlobalCfg = importcfg.NewGlobalConfig()
		importCfg = importcfg.NewConfig()

		// TODO: remove this if implement glue interface, which means lightning could use host TiDB's connection
		importGlobalCfg.TiDB.Port = 4000

		importGlobalCfg.App.StatusAddr = ":8289"
		importGlobalCfg.App.Level = tidbCfg.Log.Level
		importGlobalCfg.Security.CAPath = tidbCfg.Security.ClusterSSLCA
		importGlobalCfg.Security.CertPath = tidbCfg.Security.ClusterSSLCert
		importGlobalCfg.Security.KeyPath = tidbCfg.Security.ClusterSSLKey
		importGlobalCfg.TikvImporter.Backend = importcfg.BackendLocal // TODO(lance6716): will test tidb backend later
		importGlobalCfg.TikvImporter.SortedKVDir = filepath.Join(tidbCfg.TempStoragePath, defaultImportID)

		importCfg.Checkpoint.Schema = defaultImportID
		importCfg.Checkpoint.Driver = importcfg.CheckpointDriverMySQL
		importCfg.Mydumper.CSV.Header = false // TODO(lance6716): address this behaviour to user
		importCfg.PostRestore.Analyze = importcfg.OpLevelRequired
		// TODO(lance6716): test sql_mode
	default:
		b.err = errors.Errorf("unsupported BRIE statement kind: %s", s.Kind)
		return nil
	}

	storageURL, err := url.Parse(s.Storage)
	if err != nil {
		b.err = errors.Annotate(err, "invalid destination URL")
		return nil
	}

	switch s.Kind {
	case ast.BRIEKindBackup, ast.BRIEKindRestore:
		switch storageURL.Scheme {
		case "s3":
			storage.ExtractQueryParameters(storageURL, &brCfg.S3)
		case "gs", "gcs":
			storage.ExtractQueryParameters(storageURL, &brCfg.GCS)
		default:
			break
		}
		brCfg.Storage = storageURL.String()
	case ast.BRIEKindImport:
		importGlobalCfg.Mydumper.SourceDir = storageURL.String()
	}

	e.info.storage = storageURL.String()

	if s.Kind == ast.BRIEKindBackup || s.Kind == ast.BRIEKindRestore {
		for _, opt := range s.Options {
			switch opt.Tp {
			case ast.BRIEOptionRateLimit:
				brCfg.RateLimit = opt.UintValue
			case ast.BRIEOptionConcurrency:
				brCfg.Concurrency = uint32(opt.UintValue)
			case ast.BRIEOptionChecksum:
				brCfg.Checksum = opt.UintValue != 0
			case ast.BRIEOptionSendCreds:
				brCfg.SendCreds = opt.UintValue != 0
			}
		}
	}

	switch s.Kind {
	case ast.BRIEKindBackup, ast.BRIEKindRestore:
		switch {
		case len(s.Tables) != 0:
			tables := make([]filter.Table, 0, len(s.Tables))
			for _, tbl := range s.Tables {
				tables = append(tables, filter.Table{Name: tbl.Name.O, Schema: tbl.Schema.O})
			}
			brCfg.TableFilter = filter.NewTablesFilter(tables...)
		case len(s.Schemas) != 0:
			brCfg.TableFilter = filter.NewSchemasFilter(s.Schemas...)
		default:
			brCfg.TableFilter = filter.All()
		}

		if tidbCfg.LowerCaseTableNames != 0 {
			brCfg.TableFilter = filter.CaseInsensitive(brCfg.TableFilter)
		}
	case ast.BRIEKindImport:
		switch {
		case len(s.Tables) != 0:
			tbls := make([]string, 0, len(s.Tables))
			for _, tbl := range s.Tables {
				if tbl.Schema.L == "" {
					b.err = errors.Errorf("please specify schema for %s in IMPORT", tbl.Name.O)
					return nil
				}
				tbls = append(tbls, fmt.Sprintf("%s.%s", tbl.Schema, tbl.Name))
			}
			importGlobalCfg.Mydumper.Filter = tbls
		case len(s.Schemas) != 0:
			dbs := make([]string, 0, len(s.Schemas))
			for _, db := range s.Schemas {
				dbs = append(dbs, fmt.Sprintf("%s.*", db))
			}
			importGlobalCfg.Mydumper.Filter = dbs
		}
	}

	switch s.Kind {
	case ast.BRIEKindBackup:
		e.backupCfg = &task.BackupConfig{Config: brCfg}

		for _, opt := range s.Options {
			switch opt.Tp {
			case ast.BRIEOptionLastBackupTS:
				tso, err := b.parseTSString(opt.StrValue)
				if err != nil {
					b.err = err
					return nil
				}
				e.backupCfg.LastBackupTS = tso
			case ast.BRIEOptionLastBackupTSO:
				e.backupCfg.LastBackupTS = opt.UintValue
			case ast.BRIEOptionBackupTimeAgo:
				e.backupCfg.TimeAgo = time.Duration(opt.UintValue)
			case ast.BRIEOptionBackupTSO:
				e.backupCfg.BackupTS = opt.UintValue
			case ast.BRIEOptionBackupTS:
				tso, err := b.parseTSString(opt.StrValue)
				if err != nil {
					b.err = err
					return nil
				}
				e.backupCfg.BackupTS = tso
			}
		}

	case ast.BRIEKindRestore:
		e.restoreCfg = &task.RestoreConfig{Config: brCfg}
		for _, opt := range s.Options {
			switch opt.Tp {
			case ast.BRIEOptionOnline:
				e.restoreCfg.Online = opt.UintValue != 0
			}
		}

	case ast.BRIEKindImport:
		for _, opt := range s.Options {
			switch opt.Tp {
			case ast.BRIEOptionSkipSchemaFiles:
				importGlobalCfg.Mydumper.NoSchema = opt.UintValue != 0
			case ast.BRIEOptionStrictFormat:
				importCfg.Mydumper.StrictFormat = opt.UintValue != 0
			case ast.BRIEOptionCSVSeparator:
				importCfg.Mydumper.CSV.Separator = opt.StrValue
			case ast.BRIEOptionCSVDelimiter:
				importCfg.Mydumper.CSV.Delimiter = opt.StrValue
			case ast.BRIEOptionCSVHeader:
				if opt.UintValue == ast.BRIECSVHeaderIsColumns {
					importCfg.Mydumper.CSV.Header = true
				} else {
					b.err = errors.Errorf("CSV_HEADER only support FIELDS or COLUMNS to indicate it has header")
					return nil
				}
			case ast.BRIEOptionCSVNotNull:
				importCfg.Mydumper.CSV.NotNull = opt.UintValue != 0
			case ast.BRIEOptionCSVNull:
				importCfg.Mydumper.CSV.Null = opt.StrValue
			case ast.BRIEOptionCSVBackslashEscape:
				importCfg.Mydumper.CSV.BackslashEscape = opt.UintValue != 0
			case ast.BRIEOptionCSVTrimLastSeparators:
				importCfg.Mydumper.CSV.TrimLastSep = opt.UintValue != 0
			case ast.BRIEOptionChecksum:
				// TODO(lance6717): support OpLevelOptional later
				if opt.UintValue == 0 {
					importGlobalCfg.PostRestore.Checksum = importcfg.OpLevelOff
				}
			case ast.BRIEOptionAnalyze:
				if opt.UintValue == 0 {
					importGlobalCfg.PostRestore.Analyze = importcfg.OpLevelOff
				}
			}
		}

		var buf bytes.Buffer
		err := toml.NewEncoder(&buf).Encode(importCfg)

		if err != nil {
			b.err = errors.Errorf("error build IMPORT config: %v", err)
			return nil
		}

		importGlobalCfg.ConfigFileContent = buf.Bytes()
		e.importCfg = importGlobalCfg
	}

	return e
}

// BRIEExec represents an executor for BRIE statements (BACKUP, RESTORE, etc)
type BRIEExec struct {
	baseExecutor

	backupCfg  *task.BackupConfig
	restoreCfg *task.RestoreConfig
	importCfg  *importcfg.GlobalConfig
	info       *brieTaskInfo
}

// Next implements the Executor Next interface.
func (e *BRIEExec) Next(ctx context.Context, req *chunk.Chunk) error {
	req.Reset()
	if e.info == nil {
		return nil
	}

	bq := globalBRIEQueue

	taskExecuted := false

	e.info.connID = e.ctx.GetSessionVars().ConnectionID
	e.info.queueTime = types.CurrentTime(mysql.TypeDatetime)
	// TODO(lance6716): flush this item periodically
	taskCtx, taskID, item, err := bq.registerTask(ctx, e.info, e.ctx)
	defer func() {
		if !taskExecuted {
			bq.cancelTask(ctx, taskID, e.ctx)
		}
		item.cancel()
	}()
	if err != nil {
		return err
	}

	// manually monitor the Killed status...
	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if atomic.LoadUint32(&e.ctx.GetSessionVars().Killed) == 1 {
					bq.cancelTask(ctx, taskID, e.ctx)
					item.cancel()
					return
				}
			case <-taskCtx.Done():
				return
			}
		}
	}()

	err = bq.acquireTask(taskCtx, taskID, e.ctx)
	if err != nil {
		return err
	}
	defer bq.releaseTask()

	e.info.execTime = types.CurrentTime(mysql.TypeDatetime)
	glue := &tidbGlueSession{se: e.ctx, progress: item.progress, info: e.info}

	switch e.info.kind {
	case ast.BRIEKindBackup:
		err = handleBRIEError(task.RunBackup(taskCtx, glue, "Backup", e.backupCfg), ErrBRIEBackupFailed)
	case ast.BRIEKindRestore:
		err = handleBRIEError(task.RunRestore(taskCtx, glue, "Restore", e.restoreCfg), ErrBRIERestoreFailed)
	case ast.BRIEKindImport:
		// TODO(lance6716): use taskID to build a unique checkpoint/sort-kv-dir, and pass taskCtx
		l := lightning.New(e.importCfg)
		err = handleBRIEError(l.RunOnce(), ErrBRIEImportFailed)
	default:
		return errors.Errorf("unsupported BRIE statement kind: %s", e.info.kind)
	}
	taskExecuted = true
	if err != nil {
		return err
	}

	req.AppendString(0, e.info.storage)
	req.AppendUint64(1, e.info.archiveSize)
	req.AppendUint64(2, e.info.backupTS)
	req.AppendTime(3, e.info.queueTime)
	req.AppendTime(4, e.info.execTime)
	e.info = nil
	return nil
}

func handleBRIEError(err error, terror *terror.Error) error {
	if err == nil {
		return nil
	}
	return terror.GenWithStackByArgs(err)
}

func (e *ShowExec) fetchShowBRIE(ctx context.Context, kind ast.BRIEKind) error {
	sql := fmt.Sprintf(`SELECT data_path, status, progress, queue_time, exec_time, finish_time, conn_id
			FROM mysql.brie_tasks WHERE kind = '%s';`, kind.String())
	rs, err := e.ctx.(sqlexec.SQLExecutor).Execute(ctx, sql)
	if err != nil {
		return err
	}
	r := rs[0]
	defer terror.Call(r.Close)
	req := r.NewChunk()
	it := chunk.NewIterator4Chunk(req)
	for {
		err = r.Next(ctx, req)
		if err != nil {
			return err
		}
		if req.NumRows() == 0 {
			break
		}

		for row := it.Begin(); row != it.End(); row = it.Next() {
			dataPath, err := base64.StdEncoding.DecodeString(row.GetString(0))
			if err != nil {
				logutil.Logger(ctx).Error("failed to decode base64", zap.Error(err))
				continue
			}

			e.result.AppendBytes(0, dataPath)
			e.result.AppendString(1, row.GetString(1))
			e.result.AppendFloat64(2, row.GetFloat64(2))
			e.result.AppendTime(3, row.GetTime(3))
			e.result.AppendTime(4, row.GetTime(4))
			e.result.AppendTime(5, row.GetTime(5))
			e.result.AppendUint64(6, row.GetUint64(6))
		}
	}
	return nil
}

type tidbGlueSession struct {
	se       sessionctx.Context
	progress *brieTaskProgress
	info     *brieTaskInfo
}

// GetDomain implements glue.Glue
func (gs *tidbGlueSession) GetDomain(store kv.Storage) (*domain.Domain, error) {
	return domain.GetDomain(gs.se), nil
}

// CreateSession implements glue.Glue
func (gs *tidbGlueSession) CreateSession(store kv.Storage) (glue.Session, error) {
	return gs, nil
}

// Execute implements glue.Session
func (gs *tidbGlueSession) Execute(ctx context.Context, sql string) error {
	_, err := gs.se.(sqlexec.SQLExecutor).Execute(ctx, sql)
	return err
}

// CreateDatabase implements glue.Session
func (gs *tidbGlueSession) CreateDatabase(ctx context.Context, schema *model.DBInfo) error {
	d := domain.GetDomain(gs.se).DDL()
	schema = schema.Clone()
	if len(schema.Charset) == 0 {
		schema.Charset = mysql.DefaultCharset
	}
	return d.CreateSchemaWithInfo(gs.se, schema, ddl.OnExistIgnore, true)
}

// CreateTable implements glue.Session
func (gs *tidbGlueSession) CreateTable(ctx context.Context, dbName model.CIStr, table *model.TableInfo) error {
	d := domain.GetDomain(gs.se).DDL()

	// Clone() does not clone partitions yet :(
	table = table.Clone()
	if table.Partition != nil {
		newPartition := *table.Partition
		newPartition.Definitions = append([]model.PartitionDefinition{}, table.Partition.Definitions...)
		table.Partition = &newPartition
	}

	return d.CreateTableWithInfo(gs.se, dbName, table, ddl.OnExistIgnore, true)
}

// Close implements glue.Session
func (gs *tidbGlueSession) Close() {
}

// Open implements glue.Glue
func (gs *tidbGlueSession) Open(string, pd.SecurityOption) (kv.Storage, error) {
	return gs.se.GetStore(), nil
}

// OwnsStorage implements glue.Glue
func (gs *tidbGlueSession) OwnsStorage() bool {
	return false
}

// StartProgress implements glue.Glue
func (gs *tidbGlueSession) StartProgress(ctx context.Context, cmdName string, total int64, redirectLog bool) glue.Progress {
	// TODO(lance6716): maybe periodically flush to tikv
	gs.progress.lock.Lock()
	gs.progress.cmd = cmdName
	gs.progress.total = total
	atomic.StoreInt64(&gs.progress.current, 0)
	gs.progress.lock.Unlock()
	return gs.progress
}

// Record implements glue.Glue
func (gs *tidbGlueSession) Record(name string, value uint64) {
	switch name {
	case "BackupTS":
		gs.info.backupTS = value
	case "Size":
		gs.info.archiveSize = value
	}
}
