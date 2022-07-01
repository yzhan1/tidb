// Copyright 2022 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package isolation

import (
	"context"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/infoschema"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/parser/ast"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/sessiontxn"
	"github.com/pingcap/tidb/sessiontxn/staleread"
	"github.com/pingcap/tidb/table/temptable"
	"github.com/tikv/client-go/v2/oracle"
)

// baseTxnContextProvider is a base class for the transaction context providers that implement `TxnContextProvider` in different isolation.
// It provides some common functions below:
//   - Provides a default `OnInitialize` method to initialize its inner state.
//   - Provides some methods like `activateTxn` and `prepareTxn` to manage the inner transaction.
//   - Provides default methods `GetTxnInfoSchema`, `GetStmtReadTS` and `GetStmtForUpdateTS` and return the snapshot information schema or ts when `tidb_snapshot` is set.
//   - Provides other default methods like `Advise`, `OnStmtStart`, `OnStmtRetry` and `OnStmtErrorForNextAction`
// The subclass can set some inner property of `baseTxnContextProvider` when it is constructed.
// For example, `getStmtReadTSFunc` and `getStmtForUpdateTSFunc` should be set, and they will be called when `GetStmtReadTS`
// or `GetStmtForUpdate` to get the timestamp that should be used by the corresponding isolation level.
type baseTxnContextProvider struct {
	// States that should be initialized when baseTxnContextProvider is created and should not be changed after that
	sctx                   sessionctx.Context
	causalConsistencyOnly  bool
	onInitializeTxnCtx     func(*variable.TransactionContext)
	onTxnActive            func(kv.Transaction, sessiontxn.EnterNewTxnType)
	getStmtReadTSFunc      func() (uint64, error)
	getStmtForUpdateTSFunc func() (uint64, error)

	// Runtime states
	ctx             context.Context
	infoSchema      infoschema.InfoSchema
	txn             kv.Transaction
	isTxnPrepared   bool
	enterNewTxnType sessiontxn.EnterNewTxnType
}

// OnInitialize is the hook that should be called when enter a new txn with this provider
func (p *baseTxnContextProvider) OnInitialize(ctx context.Context, tp sessiontxn.EnterNewTxnType) (err error) {
	if p.getStmtReadTSFunc == nil || p.getStmtForUpdateTSFunc == nil {
		return errors.New("ts functions should not be nil")
	}

	p.ctx = ctx
	sessVars := p.sctx.GetSessionVars()
	activeNow := true
	switch tp {
	case sessiontxn.EnterNewTxnDefault:
		if err = p.sctx.NewTxn(ctx); err != nil {
			return err
		}
	case sessiontxn.EnterNewTxnWithBeginStmt:
		if !sessiontxn.CanReuseTxnWhenExplicitBegin(p.sctx) {
			if err = p.sctx.NewTxn(ctx); err != nil {
				return err
			}
		}
		sessVars.SetInTxn(true)
	case sessiontxn.EnterNewTxnBeforeStmt:
		activeNow = false
	default:
		return errors.Errorf("Unsupported type: %v", tp)
	}

	p.enterNewTxnType = tp
	p.infoSchema = p.sctx.GetDomainInfoSchema().(infoschema.InfoSchema)
	txnCtx := &variable.TransactionContext{
		TxnCtxNoNeedToRestore: variable.TxnCtxNoNeedToRestore{
			CreateTime: time.Now(),
			InfoSchema: p.infoSchema,
			ShardStep:  int(sessVars.ShardAllocateStep),
			TxnScope:   sessVars.CheckAndGetTxnScope(),
		},
	}
	if p.onInitializeTxnCtx != nil {
		p.onInitializeTxnCtx(txnCtx)
	}
	sessVars.TxnCtx = txnCtx

	txn, err := p.sctx.Txn(false)
	if err != nil {
		return err
	}
	p.isTxnPrepared = txn.Valid() || p.sctx.GetPreparedTxnFuture() != nil
	if activeNow {
		_, err = p.ActivateTxn()
	}

	return err
}

func (p *baseTxnContextProvider) GetTxnInfoSchema() infoschema.InfoSchema {
	if is := p.sctx.GetSessionVars().SnapshotInfoschema; is != nil {
		return is.(infoschema.InfoSchema)
	}
	return p.infoSchema
}

func (p *baseTxnContextProvider) GetStmtReadTS() (uint64, error) {
	if _, err := p.ActivateTxn(); err != nil {
		return 0, err
	}

	if snapshotTS := p.sctx.GetSessionVars().SnapshotTS; snapshotTS != 0 {
		return snapshotTS, nil
	}
	return p.getStmtReadTSFunc()
}

func (p *baseTxnContextProvider) GetStmtForUpdateTS() (uint64, error) {
	if _, err := p.ActivateTxn(); err != nil {
		return 0, err
	}

	if snapshotTS := p.sctx.GetSessionVars().SnapshotTS; snapshotTS != 0 {
		return snapshotTS, nil
	}
	return p.getStmtForUpdateTSFunc()
}

func (p *baseTxnContextProvider) OnStmtStart(ctx context.Context, _ ast.StmtNode) error {
	p.ctx = ctx
	return nil
}

func (p *baseTxnContextProvider) OnStmtRetry(ctx context.Context) error {
	p.ctx = ctx
	return nil
}

func (p *baseTxnContextProvider) OnStmtErrorForNextAction(point sessiontxn.StmtErrorHandlePoint, err error) (sessiontxn.StmtErrorAction, error) {
	switch point {
	case sessiontxn.StmtErrAfterPessimisticLock:
		// for pessimistic lock error, return the error by default
		return sessiontxn.ErrorAction(err)
	default:
		return sessiontxn.NoIdea()
	}
}

func (p *baseTxnContextProvider) getTxnStartTS() (uint64, error) {
	txn, err := p.ActivateTxn()
	if err != nil {
		return 0, err
	}
	return txn.StartTS(), nil
}

func (p *baseTxnContextProvider) ActivateTxn() (kv.Transaction, error) {
	if p.txn != nil {
		return p.txn, nil
	}

	if err := p.prepareTxn(); err != nil {
		return nil, err
	}

	txnFuture := p.sctx.GetPreparedTxnFuture()
	if txnFuture == nil {
		return nil, errors.AddStack(kv.ErrInvalidTxn)
	}

	txn, err := txnFuture.Wait(p.ctx, p.sctx)
	if err != nil {
		return nil, err
	}

	sessVars := p.sctx.GetSessionVars()
	sessVars.TxnCtx.StartTS = txn.StartTS()

	if p.enterNewTxnType == sessiontxn.EnterNewTxnBeforeStmt && !sessVars.IsAutocommit() && sessVars.SnapshotTS == 0 {
		sessVars.SetInTxn(true)
	}

	txn.SetVars(sessVars.KVVars)

	readReplicaType := sessVars.GetReplicaRead()
	if readReplicaType.IsFollowerRead() {
		txn.SetOption(kv.ReplicaRead, readReplicaType)
	}
	txn.SetOption(kv.SnapInterceptor, temptable.SessionSnapshotInterceptor(p.sctx))

	if sessVars.StmtCtx.WeakConsistency {
		txn.SetOption(kv.IsolationLevel, kv.RC)
	}

	sessiontxn.SetTxnAssertionLevel(txn, sessVars.AssertionLevel)

	if p.causalConsistencyOnly {
		txn.SetOption(kv.GuaranteeLinearizability, false)
	}

	if p.onTxnActive != nil {
		p.onTxnActive(txn, p.enterNewTxnType)
	}

	if p.sctx.GetSessionVars().InRestrictedSQL {
		txn.SetOption(kv.RequestSourceInternal, true)
	}

	if tp := p.sctx.GetSessionVars().RequestSourceType; tp != "" {
		txn.SetOption(kv.RequestSourceType, tp)
	}

	p.txn = txn
	return txn, nil
}

func (p *baseTxnContextProvider) prepareTxn() error {
	if p.isTxnPrepared {
		return nil
	}

	if snapshotTS := p.sctx.GetSessionVars().SnapshotTS; snapshotTS != 0 {
		return p.prepareTxnWithTS(snapshotTS)
	}

	future := sessiontxn.NewOracleFuture(p.ctx, p.sctx, p.sctx.GetSessionVars().TxnCtx.TxnScope)
	return p.replaceTxnTsFuture(future)
}

func (p *baseTxnContextProvider) prepareTxnWithTS(ts uint64) error {
	return p.replaceTxnTsFuture(sessiontxn.ConstantFuture(ts))
}

func (p *baseTxnContextProvider) replaceTxnTsFuture(future oracle.Future) error {
	txn, err := p.sctx.Txn(false)
	if err != nil {
		return err
	}

	if txn.Valid() {
		return nil
	}

	txnScope := p.sctx.GetSessionVars().TxnCtx.TxnScope
	if err = p.sctx.PrepareTSFuture(p.ctx, future, txnScope); err != nil {
		return err
	}

	p.isTxnPrepared = true
	return nil
}

func (p *baseTxnContextProvider) isTidbSnapshotEnabled() bool {
	return p.sctx.GetSessionVars().SnapshotTS != 0
}

// isBeginStmtWithStaleRead indicates whether the current statement is `BeginStmt` type with stale read
// Because stale read will use `staleread.StalenessTxnContextProvider` for query, so if `staleread.IsStmtStaleness()`
// returns true in other providers, it means the current statement is `BeginStmt` with stale read
func (p *baseTxnContextProvider) isBeginStmtWithStaleRead() bool {
	return staleread.IsStmtStaleness(p.sctx)
}

// AdviseWarmup provides warmup for inner state
func (p *baseTxnContextProvider) AdviseWarmup() error {
	if p.isBeginStmtWithStaleRead() {
		// When executing `START TRANSACTION READ ONLY AS OF ...` no need to warmUp
		return nil
	}
	return p.prepareTxn()
}

// AdviseOptimizeWithPlan providers optimization according to the plan
func (p *baseTxnContextProvider) AdviseOptimizeWithPlan(_ interface{}) error {
	return nil
}

// GetSnapshotWithStmtReadTS get snapshot with read ts
func (p *baseTxnContextProvider) GetSnapshotWithStmtReadTS() (kv.Snapshot, error) {
	ts, err := p.GetStmtReadTS()
	if err != nil {
		return nil, err
	}

	return p.getSnapshotByTS(ts)
}

// GetSnapshotWithStmtForUpdateTS get snapshot with for update ts
func (p *baseTxnContextProvider) GetSnapshotWithStmtForUpdateTS() (kv.Snapshot, error) {
	ts, err := p.GetStmtForUpdateTS()
	if err != nil {
		return nil, err
	}

	return p.getSnapshotByTS(ts)
}

// getSnapshotByTS get snapshot from store according to the snapshotTS and set the transaction related
// options before return
func (p *baseTxnContextProvider) getSnapshotByTS(snapshotTS uint64) (kv.Snapshot, error) {
	txn, err := p.sctx.Txn(false)
	if err != nil {
		return nil, err
	}

	txnCtx := p.sctx.GetSessionVars().TxnCtx
	if txn.Valid() && txnCtx.StartTS == txnCtx.GetForUpdateTS() && txnCtx.StartTS == snapshotTS {
		return txn.GetSnapshot(), nil
	}

	sessVars := p.sctx.GetSessionVars()
	snapshot := sessiontxn.GetSnapshotWithTS(p.sctx, snapshotTS)

	replicaReadType := sessVars.GetReplicaRead()
	if replicaReadType.IsFollowerRead() && !sessVars.StmtCtx.RCCheckTS {
		snapshot.SetOption(kv.ReplicaRead, replicaReadType)
	}

	return snapshot, nil
}
