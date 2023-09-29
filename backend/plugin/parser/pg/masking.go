package pg

import (
	"cmp"
	"fmt"
	"regexp"

	pgquery "github.com/pganalyze/pg_query_go/v4"

	"github.com/bytebase/bytebase/backend/plugin/db"
	"github.com/bytebase/bytebase/backend/plugin/parser/base"
	"github.com/bytebase/bytebase/backend/plugin/parser/sql/ast"
	pgrawparser "github.com/bytebase/bytebase/backend/plugin/parser/sql/engine/pg"
	storepb "github.com/bytebase/bytebase/proto/generated-go/store"

	"github.com/pkg/errors"
)

const (
	pgUnknownFieldName = "?column?"
)

func isSystemSchema(schema string) bool {
	switch schema {
	case "information_schema", "pg_catalog", "rw_catalog":
		return true
	}
	return false
}

type SensitiveFieldExtractor struct {
	// For Oracle, we need to know the current database to determine if the table is in the current schema.
	SchemaInfo         *db.SensitiveSchemaInfo
	outerSchemaInfo    []base.FieldInfo
	cteOuterSchemaInfo []db.TableSchema

	// SELECT statement specific field.
	fromFieldList []base.FieldInfo
}

func (extractor *SensitiveFieldExtractor) ExtractSensitiveField(statement string) ([]db.SensitiveField, error) {
	res, err := pgquery.Parse(statement)
	if err != nil {
		return nil, err
	}
	if len(res.Stmts) != 1 {
		return nil, errors.Errorf("expect one statement but found %d", len(res.Stmts))
	}
	node := res.Stmts[0]

	switch node.Stmt.Node.(type) {
	case *pgquery.Node_SelectStmt:
	case *pgquery.Node_ExplainStmt:
		// Skip the EXPLAIN statement.
		return nil, nil
	default:
		return nil, errors.Errorf("expect a query statement but found %T", node.Stmt.Node)
	}

	fieldList, err := extractor.pgExtractNode(node.Stmt)
	if err != nil {
		tableNotFound := regexp.MustCompile("^Table \"(.*)\\.(.*)\" not found$")
		content := tableNotFound.FindStringSubmatch(err.Error())
		if len(content) == 3 && isSystemSchema(content[1]) {
			// skip for system schema
			return nil, nil
		}
		return nil, err
	}

	result := []db.SensitiveField{}
	for _, field := range fieldList {
		result = append(result, db.SensitiveField{
			Name:         field.Name,
			MaskingLevel: field.MaskingLevel,
		})
	}
	return result, nil
}

func (extractor *SensitiveFieldExtractor) pgExtractNode(in *pgquery.Node) ([]base.FieldInfo, error) {
	if in == nil {
		return nil, nil
	}

	switch node := in.Node.(type) {
	case *pgquery.Node_SelectStmt:
		return extractor.pgExtractSelect(node)
	case *pgquery.Node_RangeVar:
		return extractor.pgExtractRangeVar(node)
	case *pgquery.Node_RangeSubselect:
		return extractor.pgExtractRangeSubselect(node)
	case *pgquery.Node_JoinExpr:
		return extractor.pgExtractJoin(node)
	}
	return nil, nil
}

func (extractor *SensitiveFieldExtractor) pgExtractJoin(in *pgquery.Node_JoinExpr) ([]base.FieldInfo, error) {
	leftFieldInfo, err := extractor.pgExtractNode(in.JoinExpr.Larg)
	if err != nil {
		return nil, err
	}
	rightFieldInfo, err := extractor.pgExtractNode(in.JoinExpr.Rarg)
	if err != nil {
		return nil, err
	}
	return pgMergeJoinField(in, leftFieldInfo, rightFieldInfo)
}

func pgMergeJoinField(node *pgquery.Node_JoinExpr, leftField []base.FieldInfo, rightField []base.FieldInfo) ([]base.FieldInfo, error) {
	leftFieldMap := make(map[string]base.FieldInfo)
	rightFieldMap := make(map[string]base.FieldInfo)
	var result []base.FieldInfo
	for _, field := range leftField {
		leftFieldMap[field.Name] = field
	}
	for _, field := range rightField {
		rightFieldMap[field.Name] = field
	}
	if node.JoinExpr.IsNatural {
		// Natural Join will merge the same column name field.
		for _, field := range leftField {
			// Merge the sensitive attribute for the same column name field.
			if rField, exists := rightFieldMap[field.Name]; exists && cmp.Less[storepb.MaskingLevel](field.MaskingLevel, rField.MaskingLevel) {
				field.MaskingLevel = rField.MaskingLevel
			}
			result = append(result, field)
		}

		for _, field := range rightField {
			if _, exists := leftFieldMap[field.Name]; !exists {
				result = append(result, field)
			}
		}
	} else {
		if len(node.JoinExpr.UsingClause) > 0 {
			// ... JOIN ... USING (...) will merge the column in USING.
			var usingList []string
			for _, nameNode := range node.JoinExpr.UsingClause {
				name, yes := nameNode.Node.(*pgquery.Node_String_)
				if !yes {
					return nil, errors.Errorf("expect Node_String_ but found %T", nameNode.Node)
				}
				usingList = append(usingList, name.String_.Sval)
			}
			usingMap := make(map[string]bool)
			for _, column := range usingList {
				usingMap[column] = true
			}

			for _, field := range leftField {
				_, existsInUsingMap := usingMap[field.Name]
				rField, existsInRightField := rightFieldMap[field.Name]
				// Merge the sensitive attribute for the column name field in USING.
				if existsInUsingMap && existsInRightField && cmp.Less[storepb.MaskingLevel](field.MaskingLevel, rField.MaskingLevel) {
					field.MaskingLevel = rField.MaskingLevel
				}
				result = append(result, field)
			}

			for _, field := range rightField {
				_, existsInUsingMap := usingMap[field.Name]
				_, existsInLeftField := leftFieldMap[field.Name]
				if existsInUsingMap && existsInLeftField {
					continue
				}
				result = append(result, field)
			}
		} else {
			result = append(result, leftField...)
			result = append(result, rightField...)
		}
	}

	return result, nil
}

func (extractor *SensitiveFieldExtractor) pgExtractRangeSubselect(node *pgquery.Node_RangeSubselect) ([]base.FieldInfo, error) {
	fieldList, err := extractor.pgExtractNode(node.RangeSubselect.Subquery)
	if err != nil {
		return nil, err
	}
	if node.RangeSubselect.Alias != nil {
		var result []base.FieldInfo
		aliasName, columnNameList, err := pgExtractAlias(node.RangeSubselect.Alias)
		if err != nil {
			return nil, err
		}
		if len(columnNameList) != 0 && len(columnNameList) != len(fieldList) {
			return nil, errors.Errorf("expect equal length but found %d and %d", len(columnNameList), len(fieldList))
		}
		for i, item := range fieldList {
			columnName := item.Name
			if len(columnNameList) > 0 {
				columnName = columnNameList[i]
			}
			result = append(result, base.FieldInfo{
				Schema:       "public",
				Table:        aliasName,
				Name:         columnName,
				MaskingLevel: item.MaskingLevel,
			})
		}
		return result, nil
	}
	return fieldList, nil
}

func pgExtractAlias(alias *pgquery.Alias) (string, []string, error) {
	if alias == nil {
		return "", nil, nil
	}
	var columnNameList []string
	for _, item := range alias.Colnames {
		stringNode, yes := item.Node.(*pgquery.Node_String_)
		if !yes {
			return "", nil, errors.Errorf("expect Node_String_ but found %T", item.Node)
		}
		columnNameList = append(columnNameList, stringNode.String_.Sval)
	}
	return alias.Aliasname, columnNameList, nil
}

func (extractor *SensitiveFieldExtractor) pgExtractRangeVar(node *pgquery.Node_RangeVar) ([]base.FieldInfo, error) {
	tableSchema, err := extractor.pgFindTableSchema(node.RangeVar.Schemaname, node.RangeVar.Relname)
	if err != nil {
		return nil, err
	}

	var res []base.FieldInfo
	if node.RangeVar.Alias == nil {
		for _, column := range tableSchema.ColumnList {
			res = append(res, base.FieldInfo{
				Name:         column.Name,
				Table:        tableSchema.Name,
				MaskingLevel: column.MaskingLevel,
			})
		}
	} else {
		aliasName, columnNameList, err := pgExtractAlias(node.RangeVar.Alias)
		if err != nil {
			return nil, err
		}
		if len(columnNameList) != 0 && len(columnNameList) != len(tableSchema.ColumnList) {
			return nil, errors.Errorf("expect equal length but found %d and %d", len(node.RangeVar.Alias.Colnames), len(tableSchema.ColumnList))
		}

		for i, column := range tableSchema.ColumnList {
			columnName := column.Name
			if len(columnNameList) > 0 {
				columnName = columnNameList[i]
			}
			res = append(res, base.FieldInfo{
				Schema:       "public",
				Name:         columnName,
				Table:        aliasName,
				MaskingLevel: column.MaskingLevel,
			})
		}
	}

	return res, nil
}

func (extractor *SensitiveFieldExtractor) pgFindTableSchema(schemaName string, tableName string) (db.TableSchema, error) {
	// Each CTE name in one WITH clause must be unique, but we can use the same name in the different level CTE, such as:
	//
	//  with tt2 as (
	//    with tt2 as (select * from t)
	//    select max(a) from tt2)
	//  select * from tt2
	//
	// This query has two CTE can be called `tt2`, and the FROM clause 'from tt2' uses the closer tt2 CTE.
	// This is the reason we loop the slice in reversed order.
	for i := len(extractor.cteOuterSchemaInfo) - 1; i >= 0; i-- {
		table := extractor.cteOuterSchemaInfo[i]
		if table.Name == tableName {
			return table, nil
		}
	}

	for _, database := range extractor.SchemaInfo.DatabaseList {
		for _, schema := range database.SchemaList {
			if schemaName == "" && schema.Name == "public" || schemaName == schema.Name {
				for _, table := range schema.TableList {
					if tableName == table.Name {
						return table, nil
					}
				}
			}
		}
	}
	return db.TableSchema{}, errors.Errorf("Table %q not found", tableName)
}

func (extractor *SensitiveFieldExtractor) pgExtractRecursiveCTE(node *pgquery.Node_CommonTableExpr) (db.TableSchema, error) {
	switch selectNode := node.CommonTableExpr.Ctequery.Node.(type) {
	case *pgquery.Node_SelectStmt:
		if selectNode.SelectStmt.Op != pgquery.SetOperation_SETOP_UNION {
			return extractor.pgExtractNonRecursiveCTE(node)
		}
		// For PostgreSQL, recursive CTE will be an UNION statement, and the left node is the initial part,
		// the right node is the recursive part.
		initialField, err := extractor.pgExtractSelect(&pgquery.Node_SelectStmt{SelectStmt: selectNode.SelectStmt.Larg})
		if err != nil {
			return db.TableSchema{}, err
		}
		if len(node.CommonTableExpr.Aliascolnames) > 0 {
			if len(node.CommonTableExpr.Aliascolnames) != len(initialField) {
				return db.TableSchema{}, errors.Errorf("The common table expression and column names list have different column counts")
			}
			for i, nameNode := range node.CommonTableExpr.Aliascolnames {
				stringNode, yes := nameNode.Node.(*pgquery.Node_String_)
				if !yes {
					return db.TableSchema{}, errors.Errorf("expect Node_String_ but found %T", nameNode.Node)
				}
				initialField[i].Name = stringNode.String_.Sval
			}
		}

		cteInfo := db.TableSchema{Name: node.CommonTableExpr.Ctename}
		for _, field := range initialField {
			cteInfo.ColumnList = append(cteInfo.ColumnList, db.ColumnInfo{
				Name:         field.Name,
				MaskingLevel: field.MaskingLevel,
			})
		}

		// Compute dependent closures.
		// There are two ways to compute dependent closures:
		//   1. find the all dependent edges, then use graph theory traversal to find the closure.
		//   2. Iterate to simulate the CTE recursive process, each turn check whether the Sensitive state has changed, and stop if no change.
		//
		// Consider the option 2 can easy to implementation, because the simulate process has been written.
		// On the other hand, the number of iterations of the entire algorithm will not exceed the length of fields.
		// In actual use, the length of fields will not be more than 20 generally.
		// So I think it's OK for now.
		// If any performance issues in use, optimize here.
		extractor.cteOuterSchemaInfo = append(extractor.cteOuterSchemaInfo, cteInfo)
		defer func() {
			extractor.cteOuterSchemaInfo = extractor.cteOuterSchemaInfo[:len(extractor.cteOuterSchemaInfo)-1]
		}()
		for {
			fieldList, err := extractor.pgExtractSelect(&pgquery.Node_SelectStmt{SelectStmt: selectNode.SelectStmt.Rarg})
			if err != nil {
				return db.TableSchema{}, err
			}
			if len(fieldList) != len(cteInfo.ColumnList) {
				return db.TableSchema{}, errors.Errorf("The common table expression and column names list have different column counts")
			}

			changed := false
			for i, field := range fieldList {
				if cmp.Less[storepb.MaskingLevel](cteInfo.ColumnList[i].MaskingLevel, field.MaskingLevel) {
					changed = true
					cteInfo.ColumnList[i].MaskingLevel = field.MaskingLevel
				}
			}

			if !changed {
				break
			}
			extractor.cteOuterSchemaInfo[len(extractor.cteOuterSchemaInfo)-1] = cteInfo
		}
		return cteInfo, nil
	default:
		return extractor.pgExtractNonRecursiveCTE(node)
	}
}

func (extractor *SensitiveFieldExtractor) pgExtractNonRecursiveCTE(node *pgquery.Node_CommonTableExpr) (db.TableSchema, error) {
	fieldList, err := extractor.pgExtractNode(node.CommonTableExpr.Ctequery)
	if err != nil {
		return db.TableSchema{}, err
	}
	if len(node.CommonTableExpr.Aliascolnames) > 0 {
		if len(node.CommonTableExpr.Aliascolnames) != len(fieldList) {
			return db.TableSchema{}, errors.Errorf("The common table expression and column names list have different column counts")
		}
		var nameList []string
		for _, nameNode := range node.CommonTableExpr.Aliascolnames {
			stringNode, yes := nameNode.Node.(*pgquery.Node_String_)
			if !yes {
				return db.TableSchema{}, errors.Errorf("expect Node_String_ but found %T", nameNode.Node)
			}
			nameList = append(nameList, stringNode.String_.Sval)
		}
		for i := 0; i < len(fieldList); i++ {
			fieldList[i].Name = nameList[i]
		}
	}
	result := db.TableSchema{
		Name:       node.CommonTableExpr.Ctename,
		ColumnList: []db.ColumnInfo{},
	}

	for _, field := range fieldList {
		result.ColumnList = append(result.ColumnList, db.ColumnInfo{
			Name:         field.Name,
			MaskingLevel: field.MaskingLevel,
		})
	}

	return result, nil
}

func (extractor *SensitiveFieldExtractor) pgExtractSelect(node *pgquery.Node_SelectStmt) ([]base.FieldInfo, error) {
	if node.SelectStmt.WithClause != nil {
		cteOuterLength := len(extractor.cteOuterSchemaInfo)
		defer func() {
			extractor.cteOuterSchemaInfo = extractor.cteOuterSchemaInfo[:cteOuterLength]
		}()
		for _, cte := range node.SelectStmt.WithClause.Ctes {
			in, yes := cte.Node.(*pgquery.Node_CommonTableExpr)
			if !yes {
				return nil, errors.Errorf("expect CommonTableExpr but found %T", cte.Node)
			}
			var cteTable db.TableSchema
			var err error
			if node.SelectStmt.WithClause.Recursive {
				cteTable, err = extractor.pgExtractRecursiveCTE(in)
			} else {
				cteTable, err = extractor.pgExtractNonRecursiveCTE(in)
			}
			if err != nil {
				return nil, err
			}
			extractor.cteOuterSchemaInfo = append(extractor.cteOuterSchemaInfo, cteTable)
		}
	}

	// The VALUES case.
	if len(node.SelectStmt.ValuesLists) > 0 {
		var result []base.FieldInfo
		for _, row := range node.SelectStmt.ValuesLists {
			var maskingLevelList []storepb.MaskingLevel
			list, yes := row.Node.(*pgquery.Node_List)
			if !yes {
				return nil, errors.Errorf("expect Node_List but found %T", row.Node)
			}
			for _, item := range list.List.Items {
				// TODO(zp): make pgExtractColumnRefFromExpressionNode returns masking level instead of maskingLevel.
				maskingLevel, err := extractor.pgExtractColumnRefFromExpressionNode(item)
				if err != nil {
					return nil, err
				}
				maskingLevelList = append(maskingLevelList, maskingLevel)
			}
			if len(result) == 0 {
				for i, item := range maskingLevelList {
					result = append(result, base.FieldInfo{
						Name:         fmt.Sprintf("column%d", i+1),
						MaskingLevel: item,
					})
				}
			}
		}
		return result, nil
	}

	switch node.SelectStmt.Op {
	case pgquery.SetOperation_SETOP_UNION, pgquery.SetOperation_SETOP_INTERSECT, pgquery.SetOperation_SETOP_EXCEPT:
		leftField, err := extractor.pgExtractSelect(&pgquery.Node_SelectStmt{SelectStmt: node.SelectStmt.Larg})
		if err != nil {
			return nil, err
		}
		rightField, err := extractor.pgExtractSelect(&pgquery.Node_SelectStmt{SelectStmt: node.SelectStmt.Rarg})
		if err != nil {
			return nil, err
		}
		if len(leftField) != len(rightField) {
			return nil, errors.Errorf("each UNION/INTERSECT/EXCEPT query must have the same number of columns")
		}
		var result []base.FieldInfo
		for i, field := range leftField {
			finalLevel := base.DefaultMaskingLevel
			if cmp.Less[storepb.MaskingLevel](finalLevel, field.MaskingLevel) {
				finalLevel = field.MaskingLevel
			}
			if cmp.Less[storepb.MaskingLevel](finalLevel, rightField[i].MaskingLevel) {
				finalLevel = rightField[i].MaskingLevel
			}
			result = append(result, base.FieldInfo{
				Name:         field.Name,
				Table:        field.Table,
				MaskingLevel: finalLevel,
			})
		}
		return result, nil
	case pgquery.SetOperation_SETOP_NONE:
	default:
		return nil, errors.Errorf("unknown select op %v", node.SelectStmt.Op)
	}

	// SetOperation_SETOP_NONE case
	var fromFieldList []base.FieldInfo
	var err error
	// Extract From field list.
	for _, item := range node.SelectStmt.FromClause {
		fromFieldList, err = extractor.pgExtractNode(item)
		if err != nil {
			return nil, err
		}
		extractor.fromFieldList = fromFieldList
	}
	defer func() {
		extractor.fromFieldList = nil
	}()

	var result []base.FieldInfo

	// Extract Target field list.
	for _, field := range node.SelectStmt.TargetList {
		resTarget, yes := field.Node.(*pgquery.Node_ResTarget)
		if !yes {
			return nil, errors.Errorf("expect Node_ResTarget but found %T", field.Node)
		}
		switch fieldNode := resTarget.ResTarget.Val.Node.(type) {
		case *pgquery.Node_ColumnRef:
			columnRef, err := pgrawparser.ConvertNodeListToColumnNameDef(fieldNode.ColumnRef.Fields)
			if err != nil {
				return nil, err
			}
			if columnRef.ColumnName == "*" {
				// SELECT * FROM ... case.
				if columnRef.Table.Name == "" {
					result = append(result, fromFieldList...)
				} else {
					schemaName, tableName, _ := extractSchemaTableColumnName(columnRef)
					for _, fromField := range fromFieldList {
						if fromField.Schema == schemaName && fromField.Table == tableName {
							result = append(result, fromField)
						}
					}
				}
			} else {
				maskingLevel, err := extractor.pgExtractColumnRefFromExpressionNode(resTarget.ResTarget.Val)
				if err != nil {
					return nil, err
				}
				columnName := columnRef.ColumnName
				if resTarget.ResTarget.Name != "" {
					columnName = resTarget.ResTarget.Name
				}
				result = append(result, base.FieldInfo{
					Name:         columnName,
					MaskingLevel: maskingLevel,
				})
			}
		default:
			maskingLevel, err := extractor.pgExtractColumnRefFromExpressionNode(resTarget.ResTarget.Val)
			if err != nil {
				return nil, err
			}
			fieldName := resTarget.ResTarget.Name
			if fieldName == "" {
				if fieldName, err = pgExtractFieldName(resTarget.ResTarget.Val); err != nil {
					return nil, err
				}
			}
			result = append(result, base.FieldInfo{
				Name:         fieldName,
				MaskingLevel: maskingLevel,
			})
		}
	}

	return result, nil
}

func pgExtractFieldName(in *pgquery.Node) (string, error) {
	if in == nil || in.Node == nil {
		return pgUnknownFieldName, nil
	}
	switch node := in.Node.(type) {
	case *pgquery.Node_ResTarget:
		if node.ResTarget.Name != "" {
			return node.ResTarget.Name, nil
		}
		return pgExtractFieldName(node.ResTarget.Val)
	case *pgquery.Node_ColumnRef:
		columnRef, err := pgrawparser.ConvertNodeListToColumnNameDef(node.ColumnRef.Fields)
		if err != nil {
			return "", err
		}
		return columnRef.ColumnName, nil
	case *pgquery.Node_FuncCall:
		lastNode, yes := node.FuncCall.Funcname[len(node.FuncCall.Funcname)-1].Node.(*pgquery.Node_String_)
		if !yes {
			return "", errors.Errorf("expect Node_string_ but found %T", node.FuncCall.Funcname[len(node.FuncCall.Funcname)-1].Node)
		}
		return lastNode.String_.Sval, nil
	case *pgquery.Node_XmlExpr:
		switch node.XmlExpr.Op {
		case pgquery.XmlExprOp_IS_XMLCONCAT:
			return "xmlconcat", nil
		case pgquery.XmlExprOp_IS_XMLELEMENT:
			return "xmlelement", nil
		case pgquery.XmlExprOp_IS_XMLFOREST:
			return "xmlforest", nil
		case pgquery.XmlExprOp_IS_XMLPARSE:
			return "xmlparse", nil
		case pgquery.XmlExprOp_IS_XMLPI:
			return "xmlpi", nil
		case pgquery.XmlExprOp_IS_XMLROOT:
			return "xmlroot", nil
		case pgquery.XmlExprOp_IS_XMLSERIALIZE:
			return "xmlserialize", nil
		case pgquery.XmlExprOp_IS_DOCUMENT:
			return pgUnknownFieldName, nil
		}
	case *pgquery.Node_TypeCast:
		// return the arg name
		columnName, err := pgExtractFieldName(node.TypeCast.Arg)
		if err != nil {
			return "", err
		}
		if columnName != pgUnknownFieldName {
			return columnName, nil
		}
		// return the type name
		if node.TypeCast.TypeName != nil {
			lastName, yes := node.TypeCast.TypeName.Names[len(node.TypeCast.TypeName.Names)-1].Node.(*pgquery.Node_String_)
			if !yes {
				return "", errors.Errorf("expect Node_string_ but found %T", node.TypeCast.TypeName.Names[len(node.TypeCast.TypeName.Names)-1].Node)
			}
			return lastName.String_.Sval, nil
		}
	case *pgquery.Node_AConst:
		return pgUnknownFieldName, nil
	case *pgquery.Node_AExpr:
		return pgUnknownFieldName, nil
	case *pgquery.Node_CaseExpr:
		return "case", nil
	case *pgquery.Node_AArrayExpr:
		return "array", nil
	case *pgquery.Node_NullTest:
		return pgUnknownFieldName, nil
	case *pgquery.Node_XmlSerialize:
		return "xmlserialize", nil
	case *pgquery.Node_ParamRef:
		return pgUnknownFieldName, nil
	case *pgquery.Node_BoolExpr:
		return pgUnknownFieldName, nil
	case *pgquery.Node_SubLink:
		switch node.SubLink.SubLinkType {
		case pgquery.SubLinkType_EXISTS_SUBLINK:
			return "exists", nil
		case pgquery.SubLinkType_ARRAY_SUBLINK:
			return "array", nil
		case pgquery.SubLinkType_EXPR_SUBLINK:
			if node.SubLink.Subselect != nil {
				selectNode, yes := node.SubLink.Subselect.Node.(*pgquery.Node_SelectStmt)
				if !yes {
					return pgUnknownFieldName, nil
				}
				if len(selectNode.SelectStmt.TargetList) == 1 {
					return pgExtractFieldName(selectNode.SelectStmt.TargetList[0])
				}
				return pgUnknownFieldName, nil
			}
		default:
			return pgUnknownFieldName, nil
		}
	case *pgquery.Node_RowExpr:
		return "row", nil
	case *pgquery.Node_CoalesceExpr:
		return "coalesce", nil
	case *pgquery.Node_SetToDefault:
		return pgUnknownFieldName, nil
	case *pgquery.Node_AIndirection:
		// TODO(rebelice): we do not deal with the A_Indirection. Fix it.
		return pgUnknownFieldName, nil
	case *pgquery.Node_CollateClause:
		return pgExtractFieldName(node.CollateClause.Arg)
	case *pgquery.Node_CurrentOfExpr:
		return pgUnknownFieldName, nil
	case *pgquery.Node_SqlvalueFunction:
		switch node.SqlvalueFunction.Op {
		case pgquery.SQLValueFunctionOp_SVFOP_CURRENT_DATE:
			return "current_date", nil
		case pgquery.SQLValueFunctionOp_SVFOP_CURRENT_TIME, pgquery.SQLValueFunctionOp_SVFOP_CURRENT_TIME_N:
			return "current_time", nil
		case pgquery.SQLValueFunctionOp_SVFOP_CURRENT_TIMESTAMP, pgquery.SQLValueFunctionOp_SVFOP_CURRENT_TIMESTAMP_N:
			return "current_timestamp", nil
		case pgquery.SQLValueFunctionOp_SVFOP_LOCALTIME, pgquery.SQLValueFunctionOp_SVFOP_LOCALTIME_N:
			return "localtime", nil
		case pgquery.SQLValueFunctionOp_SVFOP_LOCALTIMESTAMP, pgquery.SQLValueFunctionOp_SVFOP_LOCALTIMESTAMP_N:
			return "localtimestamp", nil
		case pgquery.SQLValueFunctionOp_SVFOP_CURRENT_ROLE:
			return "current_role", nil
		case pgquery.SQLValueFunctionOp_SVFOP_CURRENT_USER:
			return "current_user", nil
		case pgquery.SQLValueFunctionOp_SVFOP_USER:
			return "user", nil
		case pgquery.SQLValueFunctionOp_SVFOP_SESSION_USER:
			return "session_user", nil
		case pgquery.SQLValueFunctionOp_SVFOP_CURRENT_CATALOG:
			return "current_catalog", nil
		case pgquery.SQLValueFunctionOp_SVFOP_CURRENT_SCHEMA:
			return "current_schema", nil
		default:
			return pgUnknownFieldName, nil
		}
	case *pgquery.Node_MinMaxExpr:
		switch node.MinMaxExpr.Op {
		case pgquery.MinMaxOp_IS_GREATEST:
			return "greatest", nil
		case pgquery.MinMaxOp_IS_LEAST:
			return "least", nil
		default:
			return pgUnknownFieldName, nil
		}
	case *pgquery.Node_BooleanTest:
		return pgUnknownFieldName, nil
	case *pgquery.Node_GroupingFunc:
		return "grouping", nil
	}
	return pgUnknownFieldName, nil
}

func extractSchemaTableColumnName(columnName *ast.ColumnNameDef) (string, string, string) {
	return columnName.Table.Schema, columnName.Table.Name, columnName.ColumnName
}

func (extractor *SensitiveFieldExtractor) pgCheckFieldMaskingLevel(schemaName string, tableName string, fieldName string) storepb.MaskingLevel {
	// One sub-query may have multi-outer schemas and the multi-outer schemas can use the same name, such as:
	//
	//  select (
	//    select (
	//      select max(a) > x1.a from t
	//    )
	//    from t1 as x1
	//    limit 1
	//  )
	//  from t as x1;
	//
	// This query has two tables can be called `x1`, and the expression x1.a uses the closer x1 table.
	// This is the reason we loop the slice in reversed order.
	for i := len(extractor.outerSchemaInfo) - 1; i >= 0; i-- {
		field := extractor.outerSchemaInfo[i]
		if (schemaName == "" && field.Schema == "public") || schemaName == field.Schema {
			sameTable := (tableName == field.Table || tableName == "")
			sameField := (fieldName == field.Name)
			if sameTable && sameField {
				return field.MaskingLevel
			}
		}
	}

	for _, field := range extractor.fromFieldList {
		sameTable := (tableName == field.Table || tableName == "")
		sameField := (fieldName == field.Name)
		if sameTable && sameField {
			return field.MaskingLevel
		}
	}

	return base.DefaultMaskingLevel
}

func (extractor *SensitiveFieldExtractor) pgExtractColumnRefFromExpressionNode(in *pgquery.Node) (storepb.MaskingLevel, error) {
	if in == nil {
		return base.DefaultMaskingLevel, nil
	}

	switch node := in.Node.(type) {
	case *pgquery.Node_List:
		return extractor.pgExtractColumnRefFromExpressionNodeList(node.List.Items)
	case *pgquery.Node_FuncCall:
		var nodeList []*pgquery.Node
		nodeList = append(nodeList, node.FuncCall.Args...)
		nodeList = append(nodeList, node.FuncCall.AggOrder...)
		nodeList = append(nodeList, node.FuncCall.AggFilter)
		return extractor.pgExtractColumnRefFromExpressionNodeList(nodeList)
	case *pgquery.Node_SortBy:
		return extractor.pgExtractColumnRefFromExpressionNode(node.SortBy.Node)
	case *pgquery.Node_XmlExpr:
		var nodeList []*pgquery.Node
		nodeList = append(nodeList, node.XmlExpr.Args...)
		nodeList = append(nodeList, node.XmlExpr.NamedArgs...)
		return extractor.pgExtractColumnRefFromExpressionNodeList(nodeList)
	case *pgquery.Node_ResTarget:
		return extractor.pgExtractColumnRefFromExpressionNode(node.ResTarget.Val)
	case *pgquery.Node_TypeCast:
		return extractor.pgExtractColumnRefFromExpressionNode(node.TypeCast.Arg)
	case *pgquery.Node_AConst:
		return base.DefaultMaskingLevel, nil
	case *pgquery.Node_ColumnRef:
		columnNameDef, err := pgrawparser.ConvertNodeListToColumnNameDef(node.ColumnRef.Fields)
		if err != nil {
			return storepb.MaskingLevel_MASKING_LEVEL_UNSPECIFIED, err
		}
		return extractor.pgCheckFieldMaskingLevel(extractSchemaTableColumnName(columnNameDef)), nil
	case *pgquery.Node_AExpr:
		var nodeList []*pgquery.Node
		nodeList = append(nodeList, node.AExpr.Lexpr)
		nodeList = append(nodeList, node.AExpr.Rexpr)
		return extractor.pgExtractColumnRefFromExpressionNodeList(nodeList)
	case *pgquery.Node_CaseExpr:
		var nodeList []*pgquery.Node
		nodeList = append(nodeList, node.CaseExpr.Arg)
		nodeList = append(nodeList, node.CaseExpr.Args...)
		nodeList = append(nodeList, node.CaseExpr.Defresult)
		return extractor.pgExtractColumnRefFromExpressionNodeList(nodeList)
	case *pgquery.Node_CaseWhen:
		var nodeList []*pgquery.Node
		nodeList = append(nodeList, node.CaseWhen.Expr)
		nodeList = append(nodeList, node.CaseWhen.Result)
		return extractor.pgExtractColumnRefFromExpressionNodeList(nodeList)
	case *pgquery.Node_AArrayExpr:
		return extractor.pgExtractColumnRefFromExpressionNodeList(node.AArrayExpr.Elements)
	case *pgquery.Node_NullTest:
		return extractor.pgExtractColumnRefFromExpressionNode(node.NullTest.Arg)
	case *pgquery.Node_XmlSerialize:
		return extractor.pgExtractColumnRefFromExpressionNode(node.XmlSerialize.Expr)
	case *pgquery.Node_ParamRef:
		return base.DefaultMaskingLevel, nil
	case *pgquery.Node_BoolExpr:
		return extractor.pgExtractColumnRefFromExpressionNodeList(node.BoolExpr.Args)
	case *pgquery.Node_SubLink:
		maskingLevel, err := extractor.pgExtractColumnRefFromExpressionNode(node.SubLink.Testexpr)
		if err != nil {
			return storepb.MaskingLevel_MASKING_LEVEL_UNSPECIFIED, err
		}
		// Subquery in SELECT fields is special.
		// It can be the non-associated or associated subquery.
		// For associated subquery, we should set the fromFieldList as the outerSchemaInfo.
		// So that the subquery can access the outer schema.
		// The reason for new extractor is that we still need the current fromFieldList, overriding it is not expected.
		subqueryExtractor := &SensitiveFieldExtractor{
			SchemaInfo:      extractor.SchemaInfo,
			outerSchemaInfo: append(extractor.outerSchemaInfo, extractor.fromFieldList...),
		}
		fieldList, err := subqueryExtractor.pgExtractNode(node.SubLink.Subselect)
		if err != nil {
			return storepb.MaskingLevel_MASKING_LEVEL_UNSPECIFIED, err
		}
		for _, field := range fieldList {
			if cmp.Less[storepb.MaskingLevel](maskingLevel, field.MaskingLevel) {
				maskingLevel = field.MaskingLevel
			}
			if maskingLevel == base.MaxMaskingLevel {
				return maskingLevel, nil
			}
		}
		return maskingLevel, nil
	case *pgquery.Node_RowExpr:
		return extractor.pgExtractColumnRefFromExpressionNodeList(node.RowExpr.Args)
	case *pgquery.Node_CoalesceExpr:
		return extractor.pgExtractColumnRefFromExpressionNodeList(node.CoalesceExpr.Args)
	case *pgquery.Node_SetToDefault:
		return base.DefaultMaskingLevel, nil
	case *pgquery.Node_AIndirection:
		return extractor.pgExtractColumnRefFromExpressionNode(node.AIndirection.Arg)
	case *pgquery.Node_CollateClause:
		return extractor.pgExtractColumnRefFromExpressionNode(node.CollateClause.Arg)
	case *pgquery.Node_CurrentOfExpr:
		return base.DefaultMaskingLevel, nil
	case *pgquery.Node_SqlvalueFunction:
		return base.DefaultMaskingLevel, nil
	case *pgquery.Node_MinMaxExpr:
		return extractor.pgExtractColumnRefFromExpressionNodeList(node.MinMaxExpr.Args)
	case *pgquery.Node_BooleanTest:
		return extractor.pgExtractColumnRefFromExpressionNode(node.BooleanTest.Arg)
	case *pgquery.Node_GroupingFunc:
		return extractor.pgExtractColumnRefFromExpressionNodeList(node.GroupingFunc.Args)
	}
	return base.DefaultMaskingLevel, nil
}

func (extractor *SensitiveFieldExtractor) pgExtractColumnRefFromExpressionNodeList(list []*pgquery.Node) (storepb.MaskingLevel, error) {
	finalLevel := base.DefaultMaskingLevel
	for _, node := range list {
		maskingLevel, err := extractor.pgExtractColumnRefFromExpressionNode(node)
		if err != nil {
			return storepb.MaskingLevel_MASKING_LEVEL_UNSPECIFIED, err
		}
		if cmp.Less[storepb.MaskingLevel](finalLevel, maskingLevel) {
			finalLevel = maskingLevel
		}
		if finalLevel == base.MaxMaskingLevel {
			return finalLevel, nil
		}
	}
	return finalLevel, nil
}
