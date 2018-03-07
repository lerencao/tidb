// Copyright 2015 PingCAP, Inc.
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
	"fmt"

	"github.com/juju/errors"
	"github.com/opentracing/opentracing-go"
	"github.com/pingcap/tidb/ast"
	"github.com/pingcap/tidb/config"
	"github.com/pingcap/tidb/infoschema"
	"github.com/pingcap/tidb/metrics"
	"github.com/pingcap/tidb/plan"
	"github.com/pingcap/tidb/sessionctx"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
)

const expensive_plan_error = "expensive to execute"

type expensiveLevel int

const (
	notExpensive expensiveLevel = iota
	expensive
	tooExpensive
)

// Compiler compiles an ast.StmtNode to a physical plan.
type Compiler struct {
	Ctx sessionctx.Context
}

// Compile compiles an ast.StmtNode to a physical plan.
func (c *Compiler) Compile(ctx context.Context, stmtNode ast.StmtNode) (*ExecStmt, error) {
	if span := opentracing.SpanFromContext(ctx); span != nil {
		span1 := opentracing.StartSpan("executor.Compile", opentracing.ChildOf(span.Context()))
		defer span1.Finish()
	}

	infoSchema := GetInfoSchema(c.Ctx)
	if err := plan.Preprocess(c.Ctx, stmtNode, infoSchema, false); err != nil {
		return nil, errors.Trace(err)
	}

	finalPlan, err := plan.Optimize(c.Ctx, stmtNode, infoSchema)
	if err != nil {
		return nil, errors.Trace(err)
	}

	CountStmtNode(stmtNode, c.Ctx.GetSessionVars().InRestrictedSQL)
	planExpensiveLevel := logExpensiveQuery(stmtNode, finalPlan)
	if planExpensiveLevel >= tooExpensive {
		return nil, errors.Trace(errors.New(expensive_plan_error))
	}
	return &ExecStmt{
		InfoSchema: infoSchema,
		Plan:       finalPlan,
		Expensive:  planExpensiveLevel > notExpensive,
		Cacheable:  plan.Cacheable(stmtNode),
		Text:       stmtNode.Text(),
		StmtNode:   stmtNode,
		Ctx:        c.Ctx,
	}, nil
}

func logExpensiveQuery(stmtNode ast.StmtNode, finalPlan plan.Plan) (expensiveLvl expensiveLevel) {
	expensiveLvl = queryExpensiveLevel(finalPlan)
	if expensiveLvl < expensive {
		return
	}

	const logSQLLen = 1024
	sql := stmtNode.Text()
	if len(sql) > logSQLLen {
		sql = fmt.Sprintf("%s len(%d)", sql[:logSQLLen], len(sql))
	}
	log.Warnf("[EXPENSIVE_QUERY] %s", sql)
	return
}

func queryExpensiveLevel(p plan.Plan) expensiveLevel {
	switch x := p.(type) {
	case plan.PhysicalPlan:
		return physicalPlanExpensiveLevel(x)
	case *plan.Execute:
		return queryExpensiveLevel(x.Plan)
	case *plan.Insert:
		if x.SelectPlan != nil {
			return physicalPlanExpensiveLevel(x.SelectPlan)
		}
	case *plan.Delete:
		if x.SelectPlan != nil {
			return physicalPlanExpensiveLevel(x.SelectPlan)
		}
	case *plan.Update:
		if x.SelectPlan != nil {
			return physicalPlanExpensiveLevel(x.SelectPlan)
		}
	}
	return notExpensive
}

func physicalPlanExpensiveLevel(p plan.PhysicalPlan) expensiveLevel {
	var expensiveLevel = notExpensive
	expensiveRowThreshold := int64(config.GetGlobalConfig().Log.ExpensiveThreshold)
	tooExpensiveRowThreshold := int64(config.GetGlobalConfig().Log.TooExpensiveThreshold)
	if p.StatsInfo().Count() > expensiveRowThreshold {
		expensiveLevel = expensive
	}
	if tooExpensiveRowThreshold > 0 && p.StatsInfo().Count() > tooExpensiveRowThreshold {
		expensiveLevel = tooExpensive
	}

	for _, child := range p.Children() {
		childExpensiveLevel := physicalPlanExpensiveLevel(child)
		if childExpensiveLevel > expensiveLevel {
			expensiveLevel = childExpensiveLevel
		}
	}

	return expensiveLevel
}

// CountStmtNode records the number of statements with the same type.
func CountStmtNode(stmtNode ast.StmtNode, inRestrictedSQL bool) {
	if inRestrictedSQL {
		return
	}
	metrics.StmtNodeCounter.WithLabelValues(GetStmtLabel(stmtNode)).Inc()
}

// GetStmtLabel generates a label for a statement.
func GetStmtLabel(stmtNode ast.StmtNode) string {
	switch x := stmtNode.(type) {
	case *ast.AlterTableStmt:
		return "AlterTable"
	case *ast.AnalyzeTableStmt:
		return "AnalyzeTable"
	case *ast.BeginStmt:
		return "Begin"
	case *ast.CommitStmt:
		return "Commit"
	case *ast.CreateDatabaseStmt:
		return "CreateDatabase"
	case *ast.CreateIndexStmt:
		return "CreateIndex"
	case *ast.CreateTableStmt:
		return "CreateTable"
	case *ast.CreateUserStmt:
		return "CreateUser"
	case *ast.DeleteStmt:
		return "Delete"
	case *ast.DropDatabaseStmt:
		return "DropDatabase"
	case *ast.DropIndexStmt:
		return "DropIndex"
	case *ast.DropTableStmt:
		return "DropTable"
	case *ast.ExplainStmt:
		return "Explain"
	case *ast.InsertStmt:
		if x.IsReplace {
			return "Replace"
		}
		return "Insert"
	case *ast.LoadDataStmt:
		return "LoadData"
	case *ast.RollbackStmt:
		return "RollBack"
	case *ast.SelectStmt:
		return "Select"
	case *ast.SetStmt, *ast.SetPwdStmt:
		return "Set"
	case *ast.ShowStmt:
		return "Show"
	case *ast.TruncateTableStmt:
		return "TruncateTable"
	case *ast.UpdateStmt:
		return "Update"
	case *ast.GrantStmt:
		return "Grant"
	case *ast.RevokeStmt:
		return "Revoke"
	case *ast.DeallocateStmt:
		return "Deallocate"
	case *ast.ExecuteStmt:
		return "Execute"
	case *ast.PrepareStmt:
		return "Prepare"
	case *ast.UseStmt:
		return "IGNORE"
	}
	return "other"
}

// GetInfoSchema gets TxnCtx InfoSchema if snapshot schema is not set,
// Otherwise, snapshot schema is returned.
func GetInfoSchema(ctx sessionctx.Context) infoschema.InfoSchema {
	sessVar := ctx.GetSessionVars()
	var is infoschema.InfoSchema
	if snap := sessVar.SnapshotInfoschema; snap != nil {
		is = snap.(infoschema.InfoSchema)
		log.Infof("[%d] use snapshot schema %d", sessVar.ConnectionID, is.SchemaMetaVersion())
	} else {
		is = sessVar.TxnCtx.InfoSchema.(infoschema.InfoSchema)
	}
	return is
}
