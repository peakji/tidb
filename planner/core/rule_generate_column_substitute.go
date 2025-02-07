// Copyright 2019 PingCAP, Inc.
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

package core

import (
	"context"

	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/parser/ast"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/sessionctx/stmtctx"
	"github.com/pingcap/tidb/types"
)

type gcSubstituter struct {
}

// ExprColumnMap is used to store all expressions of indexed generated columns in a table,
// and map them to the generated columns,
// thus we can substitute the expression in a query to an indexed generated column.
type ExprColumnMap map[expression.Expression]*expression.Column

// optimize try to replace the expression to indexed virtual generate column in where, group by, order by, and field clause
// so that we can use the index on expression.
// For example: select a+1 from t order by a+1, with a virtual generate column c as (a+1) and
// an index on c. We need to replace a+1 with c so that we can use the index on c.
// See also https://dev.mysql.com/doc/refman/8.0/en/generated-column-index-optimizations.html
func (gc *gcSubstituter) optimize(ctx context.Context, lp LogicalPlan, _ *logicalOptimizeOp) (LogicalPlan, error) {
	exprToColumn := make(ExprColumnMap)
	collectGenerateColumn(lp, exprToColumn)
	if len(exprToColumn) == 0 {
		return lp, nil
	}
	return gc.substitute(ctx, lp, exprToColumn), nil
}

// collectGenerateColumn collect the generate column and save them to a map from their expressions to themselves.
// For the sake of simplicity, we don't collect the stored generate column because we can't get their expressions directly.
// TODO: support stored generate column.
func collectGenerateColumn(lp LogicalPlan, exprToColumn ExprColumnMap) {
	for _, child := range lp.Children() {
		collectGenerateColumn(child, exprToColumn)
	}
	ds, ok := lp.(*DataSource)
	if !ok {
		return
	}
	for _, p := range ds.possibleAccessPaths {
		if p.IsTablePath() {
			continue
		}
		for _, idxPart := range p.Index.Columns {
			colInfo := ds.tableInfo.Columns[idxPart.Offset]
			if colInfo.IsGenerated() && !colInfo.GeneratedStored {
				s := ds.schema.Columns
				col := expression.ColInfo2Col(s, colInfo)
				if col != nil && col.GetType().Equal(col.VirtualExpr.GetType()) {
					exprToColumn[col.VirtualExpr] = col
				}
			}
		}
	}
}

func tryToSubstituteExpr(expr *expression.Expression, sctx sessionctx.Context, candidateExpr expression.Expression, tp types.EvalType, schema *expression.Schema, col *expression.Column) {
	if (*expr).Equal(sctx, candidateExpr) && candidateExpr.GetType().EvalType() == tp &&
		schema.ColumnIndex(col) != -1 {
		*expr = col
	}
}

func substituteExpression(cond expression.Expression, sctx *stmtctx.StatementContext, sessionCtx sessionctx.Context, exprToColumn ExprColumnMap, schema *expression.Schema) {
	sf, ok := cond.(*expression.ScalarFunction)
	if !ok {
		return
	}
	defer func() {
		// If the argument is not changed, hash code doesn't need to recount again.
		// But we always do it to keep the code simple and stupid.
		expression.ReHashCode(sf, sctx)
	}()
	var expr *expression.Expression
	var tp types.EvalType
	switch sf.FuncName.L {
	case ast.EQ, ast.LT, ast.LE, ast.GT, ast.GE:
		for candidateExpr, column := range exprToColumn {
			tryToSubstituteExpr(&sf.GetArgs()[1], sessionCtx, candidateExpr, sf.GetArgs()[0].GetType().EvalType(), schema, column)
		}
		for candidateExpr, column := range exprToColumn {
			tryToSubstituteExpr(&sf.GetArgs()[0], sessionCtx, candidateExpr, sf.GetArgs()[1].GetType().EvalType(), schema, column)
		}
	case ast.In:
		expr = &sf.GetArgs()[0]
		tp = sf.GetArgs()[1].GetType().EvalType()
		canSubstitute := true
		// Can only substitute if all the operands on the right-hand
		// side are the same type.
		for i := 1; i < len(sf.GetArgs()); i++ {
			if sf.GetArgs()[i].GetType().EvalType() != tp {
				canSubstitute = false
				break
			}
		}
		if canSubstitute {
			for candidateExpr, column := range exprToColumn {
				tryToSubstituteExpr(expr, sessionCtx, candidateExpr, tp, schema, column)
			}
		}
	case ast.Like:
		expr = &sf.GetArgs()[0]
		tp = sf.GetArgs()[1].GetType().EvalType()
		for candidateExpr, column := range exprToColumn {
			tryToSubstituteExpr(expr, sessionCtx, candidateExpr, tp, schema, column)
		}
	case ast.LogicOr, ast.LogicAnd:
		substituteExpression(sf.GetArgs()[0], sctx, sessionCtx, exprToColumn, schema)
		substituteExpression(sf.GetArgs()[1], sctx, sessionCtx, exprToColumn, schema)
	case ast.UnaryNot:
		substituteExpression(sf.GetArgs()[0], sctx, sessionCtx, exprToColumn, schema)
	}
}

func (gc *gcSubstituter) substitute(ctx context.Context, lp LogicalPlan, exprToColumn ExprColumnMap) LogicalPlan {
	sctx := lp.SCtx().GetSessionVars().StmtCtx
	var tp types.EvalType
	switch x := lp.(type) {
	case *LogicalSelection:
		for _, cond := range x.Conditions {
			substituteExpression(cond, sctx, lp.SCtx(), exprToColumn, x.Schema())
		}
	case *LogicalProjection:
		for i := range x.Exprs {
			tp = x.Exprs[i].GetType().EvalType()
			for candidateExpr, column := range exprToColumn {
				tryToSubstituteExpr(&x.Exprs[i], lp.SCtx(), candidateExpr, tp, x.children[0].Schema(), column)
			}
		}
	case *LogicalSort:
		for i := range x.ByItems {
			tp = x.ByItems[i].Expr.GetType().EvalType()
			for candidateExpr, column := range exprToColumn {
				tryToSubstituteExpr(&x.ByItems[i].Expr, lp.SCtx(), candidateExpr, tp, x.Schema(), column)
			}
		}
	case *LogicalAggregation:
		for _, aggFunc := range x.AggFuncs {
			for i := 0; i < len(aggFunc.Args); i++ {
				tp = aggFunc.Args[i].GetType().EvalType()
				for candidateExpr, column := range exprToColumn {
					if aggFunc.Args[i].Equal(lp.SCtx(), candidateExpr) && candidateExpr.GetType().EvalType() == tp &&
						x.Schema().ColumnIndex(column) != -1 {
						aggFunc.Args[i] = column
					}
				}
			}
		}
		for i := 0; i < len(x.GroupByItems); i++ {
			tp = x.GroupByItems[i].GetType().EvalType()
			for candidateExpr, column := range exprToColumn {
				if x.GroupByItems[i].Equal(lp.SCtx(), candidateExpr) && candidateExpr.GetType().EvalType() == tp &&
					x.Schema().ColumnIndex(column) != -1 {
					x.GroupByItems[i] = column
				}
			}
		}
	}
	for _, child := range lp.Children() {
		gc.substitute(ctx, child, exprToColumn)
	}
	return lp
}

func (*gcSubstituter) name() string {
	return "generate_column_substitute"
}
