/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package migrate

import (
	"bufio"
	"database/sql"
	"fmt"
	"strings"

	"github.com/pkg/errors"
)

// dumpMeta serves as the global knowledge oracle that stores
// all the tables' info,
// all the tables' generation guide,
// the writer to output the generated RDF entries,
// the writer to output the Dgraph schema,
// and a sqlPool to read information from MySQL
type dumpMeta struct {
	tableInfos   map[string]*sqlTable
	tableGuides  map[string]*tableGuide
	dataWriter   *bufio.Writer
	schemaWriter *bufio.Writer
	sqlPool      *sql.DB

	buf strings.Builder // reusable buf for building strings, call buf.Reset before use
}

// sqlRow captures values in a SQL table row, as well as the metadata associated
// with the row
type sqlRow struct {
	values         []interface{}
	blankNodeLabel string
	tableInfo      *sqlTable
}

// escapeColumnNames wraps columnNames with backticks in order to avoid creating
// invalid sql queries in cases where a table uses a reserved keyword as a
// column name
func escapeColumnNames(columnNames []string) []string {
	var escapedColNames []string
	for _, c := range columnNames {
		escapedColNames = append(escapedColNames, fmt.Sprintf("`"+"%s"+"`", c))
	}
	return escapedColNames
}

// dumpSchema generates the Dgraph schema based on m.tableGuides
// and sends the schema to m.schemaWriter
func (m *dumpMeta) dumpSchema() error {
	for table := range m.tableGuides {
		tableInfo := m.tableInfos[table]
		for _, index := range createDgraphSchema(tableInfo) {
			_, err := m.schemaWriter.WriteString(index)
			if err != nil {
				return errors.Wrapf(err, "while writing schema")
			}
		}
	}
	return m.schemaWriter.Flush()
}

// dumpTables goes through all the tables twice. In the first time it generates RDF entries for the
// column values. In the second time, it follows the foreign key constraints in SQL tables, and
// generate the corresponding Dgraph edges.
func (m *dumpMeta) dumpTables() error {
	for table := range m.tableInfos {
		fmt.Printf("Dumping table %s\n", table)
		if err := m.dumpTable(table); err != nil {
			return errors.Wrapf(err, "while dumping table %s", table)
		}
	}

	for table := range m.tableInfos {
		fmt.Printf("Dumping table constraints %s\n", table)
		if err := m.dumpTableConstraints(table); err != nil {
			return errors.Wrapf(err, "while dumping table %s", table)
		}
	}

	return m.dataWriter.Flush()
}

// dumpTable converts the cells in a SQL table into RDF entries,
// and sends entries to the m.dataWriter
func (m *dumpMeta) dumpTable(table string) error {
	tableGuide := m.tableGuides[table]
	tableInfo := m.tableInfos[table]

	escapedColNames := escapeColumnNames(tableInfo.columnNames)

	// Escape the table name with backticks to handle reserved keywords like "check"
	query := fmt.Sprintf(`select %s from `+"`"+`%s`+"`", strings.Join(escapedColNames, ","), table)
	rows, err := m.sqlPool.Query(query)
	if err != nil {
		return err
	}
	defer rows.Close()

	// populate the predNames
	for _, column := range tableInfo.columnNames {
		tableInfo.predNames = append(tableInfo.predNames,
			predicateName(tableInfo, column))
	}

	row := &sqlRow{
		tableInfo: tableInfo,
	}

	for rows.Next() {
		// step 1: read the row's column values
		colValues, err := getColumnValues(tableInfo.columnNames, tableInfo.columnDataTypes, rows)
		if err != nil {
			return err
		}
		row.values = colValues

		// step 2: output the column values in RDF format
		row.blankNodeLabel = tableGuide.blankNode.generate(tableInfo, colValues)
		m.outputRow(row, tableInfo)

		// step 3: record mappings to the blankNodeLabel so that future tables can look up the
		// blankNodeLabel
		tableGuide.valuesRecorder.record(tableInfo, colValues, row.blankNodeLabel)
	}

	return nil
}

// dumpTableConstraints reads data from a table, and then generate RDF entries
// from a row to another row in a foreign table by following columns with foreign key constraints.
// It then sends the generated RDF entries to the m.dataWriter
func (m *dumpMeta) dumpTableConstraints(table string) error {
	tableGuide := m.tableGuides[table]
	tableInfo := m.tableInfos[table]

	escapedColNames := escapeColumnNames(tableInfo.columnNames)

	// Escape the table name with backticks to handle reserved keywords like "check"
	query := fmt.Sprintf(`select %s from `+"`"+`%s`+"`", strings.Join(escapedColNames, ","), table)
	rows, err := m.sqlPool.Query(query)
	if err != nil {
		return err
	}
	defer rows.Close()

	row := &sqlRow{
		tableInfo: tableInfo,
	}
	for rows.Next() {
		// step 1: read the row's column values
		colValues, err := getColumnValues(tableInfo.columnNames, tableInfo.columnDataTypes, rows)
		if err != nil {
			return err
		}
		row.values = colValues

		// step 2: output the constraints in RDF format
		row.blankNodeLabel = tableGuide.blankNode.generate(tableInfo, colValues)

		m.outputConstraints(row, tableInfo)
	}

	return nil
}

// outputRow takes a row with its metadata as well as the table metadata, and
// spits out one or more RDF entries to the dumpMeta's dataWriter.
// Consider the following table "salary"
// person_company varchar (50)
// person_employee_id int
// salary float
// foreign key (person_company, person_employee_id) references person (company, employee_id)

// A row with the following values in the table
// Google, 100, 50.0 (salary)
// where Google is the person_company, 100 is the employee id, and 50.0 is the salary rate
// will cause the following RDF entries to be generated
// _:salary_1 <salary_person_company> "Google" .
// _:salary_1 <salary_person_employee_id> "100" .
// _:salary_1 <salary_person_salary> "50.0" .
// _:salary_1 <salary_person_company_person_employee_id> _:person_2.
// In the RDF output, _:salary_1 is this row's blank node label;
// salary_person_company, salary_person_employee_id, and salary_person_salary
// are the predicate names constructed by appending the column names after the table name "salary".

// The last RDF entry is a Dgraph edge created by following the foreign key reference.
// Its predicate name is constructed by concatenating the table name, and each column's name in
// alphabetical order. The object _:person_2 is the blank node label from the person table,
// and it's generated through a lookup in the person table using the "ref label"
// _:person_company_Google_employee_id_100. The mapping from the ref label
// _:person_company_Google_employee_id_100 to the foreign blank node _:person_2
// is recorded through the person table's valuesRecorder.
func (m *dumpMeta) outputRow(row *sqlRow, tableInfo *sqlTable) {
	for i, colValue := range row.values {
		colName := tableInfo.columnNames[i]
		if !tableInfo.isForeignKey[colName] {
			predicate := tableInfo.predNames[i]
			m.outputPlainCell(row.blankNodeLabel, predicate, tableInfo.columnDataTypes[i], colValue)
		}
	}
}

func (m *dumpMeta) outputConstraints(row *sqlRow, tableInfo *sqlTable) {
	for _, constraint := range tableInfo.foreignKeyConstraints {
		if len(constraint.parts) == 0 {
			logger.Fatalf("The constraint should have at least one part: %v", constraint)
		}

		foreignTableName := constraint.parts[0].remoteTableName

		refLabel, err := row.getRefLabelFromConstraint(m.tableInfos[foreignTableName], constraint)
		if err != nil {
			if !quiet {
				logger.Printf("ignoring the constraint because of error "+
					"when getting ref label: %+v\n", err)
			}
			return
		}
		foreignBlankNode := m.tableGuides[foreignTableName].valuesRecorder.getBlankNode(refLabel)
		m.outputPlainCell(row.blankNodeLabel,
			getPredFromConstraint(tableInfo.tableName, separator, constraint), uidType,
			foreignBlankNode)
	}
}

// outputPlainCell sends to the writer a RDF where the subject is the blankNode
// the predicate is the predName, and the object is the colValue
func (m *dumpMeta) outputPlainCell(blankNode string, predName string, dataType dataType,
	colValue interface{}) {
	// Each cell value should be stored under a predicate
	m.buf.Reset()
	fmt.Fprintf(&m.buf, "%s <%s> ", blankNode, predName)

	switch dataType {
	case stringType:
		fmt.Fprintf(&m.buf, "%q .\n", colValue)
	case uidType:
		fmt.Fprintf(&m.buf, "%s .\n", colValue)
	default:
		objectVal, err := getValue(dataType, colValue)
		if err != nil {
			if !quiet {
				logger.Printf("ignoring object %v because of error when getting value: %v",
					colValue, err)
			}
			return
		}

		fmt.Fprintf(&m.buf, "\"%v\" .\n", objectVal)
	}

	// send the buf to writer
	fmt.Fprintf(m.dataWriter, "%s", m.buf.String())
}

// getRefLabelFromConstraint returns a ref label based on a foreign key constraint.
// Consider the foreign key constraint
// foreign key (person_company, person_employee_id) references person (company, employee_id)
// and a row with the following values in the table
// Google, 100, 50.0 (salary)
// where Google is the person_company, 100 is the employee id, and 50.0 is the salary rate
// the refLabel will use the foreign table name, foreign column names and the local row's values,
// yielding the value of _:person_company_Google_employee_id_100
func (row *sqlRow) getRefLabelFromConstraint(foreignTableInfo *sqlTable,
	constraint *fkConstraint) (string, error) {
	if constraint.foreignIndices == nil {
		foreignKeyColumnNames := make(map[string]string)
		for _, part := range constraint.parts {
			foreignKeyColumnNames[part.columnName] = part.remoteColumnName
		}

		constraint.foreignIndices = getColumnIndices(row.tableInfo,
			func(info *sqlTable, column string) bool {
				_, ok := foreignKeyColumnNames[column]
				return ok
			})

		// replace the column names to be the foreign column names
		for _, colIdx := range constraint.foreignIndices {
			colIdx.name = foreignKeyColumnNames[colIdx.name]
		}
	}

	return createLabel(&ref{
		allColumns:       foreignTableInfo.columns,
		refColumnIndices: constraint.foreignIndices,
		tableName:        foreignTableInfo.tableName,
		colValues:        row.values,
	})
}
