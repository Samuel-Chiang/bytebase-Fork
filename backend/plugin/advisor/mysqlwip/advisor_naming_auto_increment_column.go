package mysqlwip

// Framework code is generated by the generator.

import (
	"fmt"
	"regexp"

	"github.com/antlr4-go/antlr/v4"
	"github.com/pkg/errors"

	mysql "github.com/bytebase/mysql-parser"

	"github.com/bytebase/bytebase/backend/plugin/advisor"
	mysqlparser "github.com/bytebase/bytebase/backend/plugin/parser/mysql"
	storepb "github.com/bytebase/bytebase/proto/generated-go/store"
)

var (
	_ advisor.Advisor = (*NamingAutoIncrementColumnAdvisor)(nil)
)

func init() {
	// only for mysqlwip test.
	advisor.Register(storepb.Engine_ENGINE_UNSPECIFIED, advisor.MySQLNamingAutoIncrementColumnConvention, &NamingAutoIncrementColumnAdvisor{})
}

// NamingAutoIncrementColumnAdvisor is the advisor checking for auto-increment naming convention.
type NamingAutoIncrementColumnAdvisor struct {
}

// Check checks for auto-increment naming convention.
func (*NamingAutoIncrementColumnAdvisor) Check(ctx advisor.Context, _ string) ([]advisor.Advice, error) {
	stmtList, ok := ctx.AST.([]*mysqlparser.ParseResult)
	if !ok {
		return nil, errors.Errorf("failed to convert to mysql parser result")
	}

	level, err := advisor.NewStatusBySQLReviewRuleLevel(ctx.Rule.Level)
	if err != nil {
		return nil, err
	}
	format, maxLength, err := advisor.UnmarshalNamingRulePayloadAsRegexp(ctx.Rule.Payload)
	if err != nil {
		return nil, err
	}
	checker := &namingAutoIncrementColumnChecker{
		level:     level,
		title:     string(ctx.Rule.Type),
		format:    format,
		maxLength: maxLength,
	}

	for _, stmt := range stmtList {
		checker.baseLine = stmt.BaseLine
		antlr.ParseTreeWalkerDefault.Walk(checker, stmt.Tree)
	}

	if len(checker.adviceList) == 0 {
		checker.adviceList = append(checker.adviceList, advisor.Advice{
			Status:  advisor.Success,
			Code:    advisor.Ok,
			Title:   "OK",
			Content: "",
		})
	}
	return checker.adviceList, nil
}

type namingAutoIncrementColumnChecker struct {
	*mysql.BaseMySQLParserListener

	baseLine   int
	adviceList []advisor.Advice
	level      advisor.Status
	title      string
	format     *regexp.Regexp
	maxLength  int
}

func (checker *namingAutoIncrementColumnChecker) EnterCreateTable(ctx *mysql.CreateTableContext) {
	if ctx.TableName() == nil || ctx.TableElementList() == nil {
		return
	}

	_, tableName := mysqlparser.NormalizeMySQLTableName(ctx.TableName())
	for _, tableElement := range ctx.TableElementList().AllTableElement() {
		if tableElement == nil {
			continue
		}
		if tableElement.ColumnDefinition() == nil {
			continue
		}
		if tableElement.ColumnDefinition().ColumnName() == nil || tableElement.ColumnDefinition().FieldDefinition() == nil {
			continue
		}

		_, _, columnName := mysqlparser.NormalizeMySQLColumnName(tableElement.ColumnDefinition().ColumnName())
		if checker.isAutoIncrement(tableElement.ColumnDefinition().FieldDefinition()) {
			checker.handleColumn(tableName, columnName, tableElement.GetStart().GetLine())
		}
	}
}

func (*namingAutoIncrementColumnChecker) isAutoIncrement(ctx mysql.IFieldDefinitionContext) bool {
	for _, attr := range ctx.AllColumnAttribute() {
		if attr.AUTO_INCREMENT_SYMBOL() != nil {
			return true
		}
	}
	return false
}

// EnterAlterTable is called when production alterTable is entered.
func (checker *namingAutoIncrementColumnChecker) EnterAlterTable(ctx *mysql.AlterTableContext) {
	if ctx.AlterTableActions() == nil {
		return
	}
	if ctx.AlterTableActions().AlterCommandList() == nil {
		return
	}
	if ctx.AlterTableActions().AlterCommandList().AlterList() == nil {
		return
	}

	_, tableName := mysqlparser.NormalizeMySQLTableRef(ctx.TableRef())
	// alter table add column, change column, modify column.
	for _, item := range ctx.AlterTableActions().AlterCommandList().AlterList().AllAlterListItem() {
		if item == nil {
			continue
		}

		var columnName string
		switch {
		// add column
		case item.ADD_SYMBOL() != nil && item.Identifier() != nil:
			columnName = mysqlparser.NormalizeMySQLIdentifier(item.Identifier())
		// modify column
		case item.MODIFY_SYMBOL() != nil && item.ColumnInternalRef() != nil:
			columnName = mysqlparser.NormalizeMySQLColumnInternalRef(item.ColumnInternalRef())
		// change column
		case item.CHANGE_SYMBOL() != nil && item.ColumnInternalRef() != nil && item.Identifier() != nil:
			columnName = mysqlparser.NormalizeMySQLIdentifier(item.Identifier())
		}
		checker.handleColumn(tableName, columnName, ctx.GetStart().GetLine())
	}
}

func (checker *namingAutoIncrementColumnChecker) handleColumn(tableName string, columnName string, lineNumber int) {
	lineNumber += checker.baseLine
	if !checker.format.MatchString(columnName) {
		checker.adviceList = append(checker.adviceList, advisor.Advice{
			Status:  checker.level,
			Code:    advisor.NamingAutoIncrementColumnConventionMismatch,
			Title:   checker.title,
			Content: fmt.Sprintf("`%s`.`%s` mismatches auto_increment column naming convention, naming format should be %q", tableName, columnName, checker.format),
			Line:    lineNumber,
		})
	}
	if checker.maxLength > 0 && len(columnName) > checker.maxLength {
		checker.adviceList = append(checker.adviceList, advisor.Advice{
			Status:  checker.level,
			Code:    advisor.NamingAutoIncrementColumnConventionMismatch,
			Title:   checker.title,
			Content: fmt.Sprintf("`%s`.`%s` mismatches auto_increment column naming convention, its length should be within %d characters", tableName, columnName, checker.maxLength),
			Line:    lineNumber,
		})
	}
}
