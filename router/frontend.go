package app

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/pg-sharding/lyx/lyx"
	"github.com/pg-sharding/spqr/pkg/config"
	"github.com/pg-sharding/spqr/pkg/spqrlog"
	"github.com/pg-sharding/spqr/pkg/txstatus"
	"github.com/pg-sharding/spqr/pkg/workloadlog"
	"github.com/pg-sharding/spqr/router/client"
	"github.com/pg-sharding/spqr/router/parser"
	"github.com/pg-sharding/spqr/router/poolmgr"
	"github.com/pg-sharding/spqr/router/qrouter"
	"github.com/pg-sharding/spqr/router/relay"
	"github.com/pg-sharding/spqr/router/routehint"
	"github.com/pg-sharding/spqr/router/routingstate"
	"github.com/pg-sharding/spqr/router/statistics"
)

type Qinteractor interface{}

type QinteractorImpl struct{}

func AdvancedPoolModeNeeded(rst relay.RelayStateMgr) bool {
	return rst.Client().Rule().PoolMode == config.PoolModeTransaction && rst.Client().Rule().PoolPreparedStatement || rst.RouterMode() == config.ProxyMode
}

func deparseRouteHint(rst relay.RelayStateMgr, params map[string]string, dataspace string) (routehint.RouteHint, error) {
	if _, ok := params[client.SPQR_SCATTER_QUERY]; ok {
		return &routehint.ScatterRouteHint{}, nil
	}
	if val, ok := params[client.SPQR_SHARDING_KEY]; ok {
		spqrlog.Zero.Debug().Str("sharding key", val).Msg("checking hint key")

		krs, err := rst.QueryRouter().Mgr().ListKeyRanges(context.TODO(), dataspace)

		if err != nil {
			return nil, err
		}

		rls, err := rst.QueryRouter().Mgr().ListShardingRules(context.TODO(), dataspace)
		if err != nil {
			return nil, err
		}

		meta := qrouter.NewRoutingMetadataContext(krs, rls, dataspace, nil)
		ds, err := rst.QueryRouter().DeparseKeyWithRangesInternal(context.TODO(), val, meta)
		if err != nil {
			return nil, err
		}
		return &routehint.TargetRouteHint{
			State: routingstate.ShardMatchState{
				Route: ds,
			},
		}, nil
	}

	return &routehint.EmptyRouteHint{}, nil
}

func procQuery(rst relay.RelayStateMgr, query string, msg pgproto3.FrontendMessage, cmngr poolmgr.PoolMgr) error {
	statistics.RecordStartTime(statistics.Router, time.Now(), rst.Client().ID())

	spqrlog.Zero.Debug().Str("query", query).Uint("client", spqrlog.GetPointer(rst.Client()))
	state, comment, err := rst.Parse(query)
	if err != nil {
		return fmt.Errorf("error processing query '%v': %w", query, err)
	}

	mp, err := parser.ParseComment(comment)
	var routeHint routehint.RouteHint = &routehint.EmptyRouteHint{}

	if err == nil {
		routeHint, _ = deparseRouteHint(rst, mp, rst.Client().DS())

		if val, ok := mp["target-session-attrs"]; ok {
			// TBD: validate
			spqrlog.Zero.Debug().Str("tsa", val).Msg("parse tsa from comment")
			rst.Client().SetTsa(val)
		}
	}

	switch st := state.(type) {
	case parser.ParseStateTXBegin:
		if rst.TxStatus() != txstatus.TXIDLE {
			// ignore this
			_ = rst.Client().ReplyWarningf("there is already transaction in progress")
			return rst.Client().ReplyCommandComplete(rst.TxStatus(), "BEGIN")
		}
		rst.AddSilentQuery(msg)
		rst.SetTxStatus(txstatus.TXACT)
		rst.Client().StartTx()
		for _, opt := range st.Options {
			switch opt {
			case lyx.TransactionReadOnly:
				rst.Client().SetTsa(config.TargetSessionAttrsPS)
			case lyx.TransactionReadWrite:
				rst.Client().SetTsa(config.TargetSessionAttrsRW)
			}
		}
		return rst.Client().ReplyCommandComplete(rst.TxStatus(), "BEGIN")
	case parser.ParseStateTXCommit:
		if rst.TxStatus() != txstatus.TXACT {
			_ = rst.Client().ReplyWarningf("there is no transaction in progress")
			return rst.Client().ReplyCommandComplete(rst.TxStatus(), "COMMIT")
		}
		if !cmngr.ConnectionActive(rst) {
			return fmt.Errorf("client relay has no connection to shards")
		}
		rst.AddQuery(msg)
		ok, err := rst.ProcessMessageBuf(true, true, cmngr, routeHint)
		if ok {
			rst.Client().CommitActiveSet()
		}
		return err
	case parser.ParseStateTXRollback:
		if rst.TxStatus() != txstatus.TXACT {
			_ = rst.Client().ReplyWarningf("there is no transaction in progress")
			return rst.Client().ReplyCommandComplete(rst.TxStatus(), "ROLLBACK")
		}

		rst.AddQuery(msg)
		ok, err := rst.ProcessMessageBuf(true, true, cmngr, routeHint)
		if ok {
			rst.Client().Rollback()
		}
		return err
	case parser.ParseStateEmptyQuery:
		if err := rst.Client().Send(&pgproto3.EmptyQueryResponse{}); err != nil {
			return err
		}
		return rst.Client().Send(&pgproto3.ReadyForQuery{
			TxStatus: byte(rst.TxStatus()),
		})
	// with tx pooling we might have no active connection while processing set x to y
	case parser.ParseStateSetStmt:
		spqrlog.Zero.Debug().
			Str("name", st.Name).
			Str("value", st.Value).
			Msg("applying parsed set stmt")

		if strings.HasPrefix(st.Name, "__spqr__") {
			param := strings.ToLower(st.Name[8:len(st.Name)])
			rst.Client().SetParam(param, st.Value)
			_ = rst.Client().ReplyCommandComplete(rst.TxStatus(), "SET")
			return nil
		}
		rst.AddQuery(msg)
		if ok, err := rst.ProcessMessageBuf(true, true, cmngr, routeHint); err != nil {
			return err
		} else if ok {
			rst.Client().SetParam(st.Name, st.Value)
		}
		return nil
	case parser.ParseStateShowStmt:
		param := strings.ToLower(st.Name[8:len(st.Name)])

		// manually create router responce
		// here we just reply single row with single column value

		_ = rst.Client().Send(
			&pgproto3.RowDescription{
				Fields: []pgproto3.FieldDescription{
					{
						DataTypeOID: 25,
					},
				},
			},
		)
		_ = rst.Client().Send(
			&pgproto3.DataRow{
				Values: [][]byte{
					[]byte(rst.Client().Params()[param]),
				},
			},
		)
		_ = rst.Client().ReplyCommandComplete(rst.TxStatus(), "SHOW")
		return nil
	case parser.ParseStateResetStmt:
		rst.Client().ResetParam(st.Name)

		if cmngr.ConnectionActive(rst) {
			if err := rst.ProcessMessage(rst.Client().ConstructClientParams(), true, false, cmngr, routeHint); err != nil {
				return err
			}
		}

		return rst.Client().ReplyCommandComplete(rst.TxStatus(), "RESET")
	case parser.ParseStateResetMetadataStmt:
		if cmngr.ConnectionActive(rst) {
			rst.AddQuery(msg)
			_, err := rst.ProcessMessageBuf(true, true, cmngr, routeHint)
			if err != nil {
				return err
			}

			rst.Client().ResetParam(st.Setting)
			if st.Setting == "session_authorization" {
				rst.Client().ResetParam("role")
			}
			return nil
		}

		rst.Client().ResetParam(st.Setting)
		if st.Setting == "session_authorization" {
			rst.Client().ResetParam("role")
		}

		return rst.Client().ReplyCommandComplete(rst.TxStatus(), "RESET")
	case parser.ParseStateResetAllStmt:
		rst.Client().ResetAll()

		return rst.Client().ReplyCommandComplete(rst.TxStatus(), "RESET")
	case parser.ParseStateSetLocalStmt:
		if cmngr.ConnectionActive(rst) {
			rst.AddQuery(msg)
			_, err := rst.ProcessMessageBuf(true, true, cmngr, routeHint)
			return err
		}

		return rst.Client().ReplyCommandComplete(rst.TxStatus(), "SET")
	case parser.ParseStatePrepareStmt:
		// sql level prepares stmt pooling
		if AdvancedPoolModeNeeded(rst) {
			spqrlog.Zero.Debug().Msg("sql level prep statement pooling support is on")
			rst.Client().StorePreparedStatement(st.Name, st.Query)
			return nil
		} else {
			rst.AddQuery(msg)
			_, err := rst.ProcessMessageBuf(true, true, cmngr, routeHint)
			return err
		}
	case parser.ParseStateExecute:
		if AdvancedPoolModeNeeded(rst) {
			// do nothing
			rst.Client().PreparedStatementQueryByName(st.Name)
			return nil
		} else {
			rst.AddQuery(msg)
			_, err := rst.ProcessMessageBuf(true, true, cmngr, routeHint)
			return err
		}
	case parser.ParseStateExplain:
		_ = rst.Client().ReplyErrMsg("parse state explain is not implemented")
		return nil
	default:
		rst.AddQuery(msg)
		_, err := rst.ProcessMessageBuf(true, true, cmngr, routeHint)
		return err
	}
}

// ProcessMessage: process client iteration, until next transaction status idle
func ProcessMessage(qr qrouter.QueryRouter, cmngr poolmgr.PoolMgr, rst relay.RelayStateMgr, msg pgproto3.FrontendMessage) error {
	if rst.Client().Rule().PoolMode != config.PoolModeTransaction {
		switch q := msg.(type) {
		case *pgproto3.Terminate:
			return nil
		case *pgproto3.Sync:
			// copy interface
			cpQ := *q
			q = &cpQ
			return rst.ProcessMessage(q, true, true, cmngr, routehint.EmptyRouteHint{})
		case *pgproto3.FunctionCall:
			// copy interface
			cpQ := *q
			q = &cpQ
			return rst.ProcessMessage(q, true, true, cmngr, routehint.EmptyRouteHint{})
		case *pgproto3.Parse:
			// copy interface
			cpQ := *q
			q = &cpQ
			return procQuery(rst, q.Query, q, cmngr)
		case *pgproto3.Execute:
			// copy interface
			cpQ := *q
			q = &cpQ
			return rst.ProcessMessage(q, false, true, cmngr, routehint.EmptyRouteHint{})
		case *pgproto3.Bind:
			// copy interface
			cpQ := *q
			q = &cpQ
			return rst.ProcessMessage(q, false, true, cmngr, routehint.EmptyRouteHint{})
		case *pgproto3.Describe:
			// copy interface
			cpQ := *q
			q = &cpQ
			return rst.ProcessMessage(q, false, true, cmngr, routehint.EmptyRouteHint{})
		case *pgproto3.Query:
			// copy interface
			cpQ := *q
			q = &cpQ
			return procQuery(rst, q.String, q, cmngr)
		default:
			return nil
		}
	}

	switch q := msg.(type) {
	case *pgproto3.Terminate:
		return nil
	case *pgproto3.Sync:
		if err := rst.ProcessExtendedBuffer(cmngr); err != nil {
			return err
		}

		spqrlog.Zero.Debug().
			Uint("client", spqrlog.GetPointer(rst.Client())).
			Msg("client connection synced")
		return nil
	case *pgproto3.Parse:
		// copy interface
		cpQ := *q
		q = &cpQ

		rst.AddExtendedProtocMessage(q)
		return nil
	case *pgproto3.Describe:
		// copy interface
		cpQ := *q
		q = &cpQ

		rst.AddExtendedProtocMessage(q)
		return nil
	case *pgproto3.FunctionCall:
		// copy interface
		cpQ := *q
		q = &cpQ
		spqrlog.Zero.Debug().
			Uint("client", rst.Client().ID()).
			Msg("client function call: simply fire parse stmt to connection")
		return rst.ProcessMessage(q, false, true, cmngr, routehint.EmptyRouteHint{})
	case *pgproto3.Execute:
		// copy interface
		cpQ := *q
		q = &cpQ

		rst.AddExtendedProtocMessage(q)
		return nil
	case *pgproto3.Bind:
		// copy interface
		cpQ := *q
		q = &cpQ

		rst.AddExtendedProtocMessage(q)
		return nil
	case *pgproto3.Query:
		// copy interface
		cpQ := *q
		q = &cpQ
		return procQuery(rst, q.String, q, cmngr)
	default:
		return nil
	}
}

func Frontend(qr qrouter.QueryRouter, cl client.RouterClient, cmngr poolmgr.PoolMgr, rcfg *config.Router, writer workloadlog.WorkloadLog) error {
	spqrlog.Zero.Info().
		Str("user", cl.Usr()).
		Str("db", cl.DB()).
		Uint("client", spqrlog.GetPointer(cl)).
		Msg("process frontend for route")

	if rcfg.PgprotoDebug {
		_ = cl.ReplyDebugNoticef("process frontend for route %s %s", cl.Usr(), cl.DB())
	}
	rst := relay.NewRelayState(qr, cl, cmngr, rcfg)

	defer rst.Close()

	var msg pgproto3.FrontendMessage
	var err error

	for {
		msg, err = cl.Receive()
		if err != nil {
			switch err {
			case io.ErrUnexpectedEOF:
				fallthrough
			case io.EOF:
				return nil
				// ok
			default:
				return rst.UnRouteWithError(rst.ActiveShards(), err)
			}
		}

		if writer != nil && writer.IsLogging() {
			switch writer.GetMode() {
			case workloadlog.All:
				writer.RecordWorkload(msg, cl.ID())
			case workloadlog.Client:
				if writer.ClientMatches(cl.ID()) {
					writer.RecordWorkload(msg, cl.ID())
				}
			}
		}

		if err := ProcessMessage(qr, cmngr, rst, msg); err != nil {
			switch err {
			case io.ErrUnexpectedEOF:
				fallthrough
			case io.EOF:
				return nil
				// ok
			default:
				spqrlog.Zero.Error().
					Uint("client", rst.Client().ID()).Int("tx-status", int(rst.TxStatus())).Err(err).
					Msg("client iteration done with error")
				if err := rst.UnRouteWithError(rst.ActiveShards(), fmt.Errorf("client proccessing error: %v, tx status %s", err, rst.TxStatus().String())); err != nil {
					return err
				}
			}
		}
	}
}
