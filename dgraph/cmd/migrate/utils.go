/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package migrate

import (
	"bufio"
	"database/sql"
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/go-sql-driver/mysql"
	"github.com/pkg/errors"

	"github.com/hypermodeinc/dgraph/v24/x"
)

func getPool(host, port, user, password, db string) (*sql.DB,
	error) {
	return sql.Open("mysql",
		fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true", user, password, host, port, db))
}

// showTables will return a slice of table names using one of the following logic
// 1) if the parameter tables is not empty, this function will return a slice of table names
// by splitting the parameter with the separate comma
// 2) if the parameter is empty, this function will read all the tables under the given
// database and then return the result
func showTables(pool *sql.DB, tableNames string, excludeTableNames string) ([]string, error) {
	// If specific tables are provided, use them
	if len(tableNames) > 0 && len(excludeTableNames) == 0 {
		return strings.Split(tableNames, ","), nil
	}
	query := "show tables"
	rows, err := pool.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tables := make([]string, 0)
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return nil, errors.Wrapf(err, "while scanning table name")
		}
		tables = append(tables, table)
	}

	// If specific tables are provided, filter to only include those
	if len(tableNames) > 0 {
		includeTables := make(map[string]bool)
		for _, t := range strings.Split(tableNames, ",") {
			includeTables[strings.TrimSpace(t)] = true
		}
		
		filteredTables := make([]string, 0)
		for _, t := range tables {
			if includeTables[t] {
				filteredTables = append(filteredTables, t)
			}
		}
		tables = filteredTables
	}

	// Filter out excluded tables
	if len(excludeTableNames) > 0 {
		excludeTables := make(map[string]bool)
		for _, t := range strings.Split(excludeTableNames, ",") {
			excludeTables[strings.TrimSpace(t)] = true
		}
		tables = filterOutExcludedTables(tables, excludeTables)
	}

	return tables, nil
}

// filterOutExcludedTables removes tables that are in the excludeTables map
func filterOutExcludedTables(tables []string, excludeTables map[string]bool) []string {
	filteredTables := make([]string, 0)
	for _, table := range tables {
		if !excludeTables[table] {
			filteredTables = append(filteredTables, table)
		}
	}
	return filteredTables
}

type criteriaFunc func(info *sqlTable, column string) bool

// getColumnIndices first sort the columns in the table alphabetically, and then
// returns the indices of the columns satisfying the criteria function
func getColumnIndices(info *sqlTable, criteria criteriaFunc) []*columnIdx {
	indices := make([]*columnIdx, 0)
	for i, column := range info.columnNames {
		if criteria(info, column) {
			indices = append(indices, &columnIdx{
				name:  column,
				index: i,
			})
		}
	}
	return indices
}

type columnIdx struct {
	name  string // the column name
	index int    // the column index
}

func getFileWriter(filename string) (*bufio.Writer, func(), error) {
	output, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return nil, nil, err
	}

	return bufio.NewWriter(output), func() { _ = output.Close() }, nil
}

func getColumnValues(columns []string, dataTypes []dataType,
	rows *sql.Rows) ([]interface{}, error) {
	// ptrToValues takes a slice of pointers, deference them, and return the values referenced
	// by these pointers
	ptrToValues := func(ptrs []interface{}) []interface{} {
		values := make([]interface{}, 0, len(ptrs))
		for _, ptr := range ptrs {
			// dereference the pointer to get the actual value
			v := reflect.ValueOf(ptr).Elem().Interface()
			values = append(values, v)
		}
		return values
	}

	valuePtrs := make([]interface{}, 0, len(columns))
	for i := range columns {
		switch dataTypes[i] {
		case stringType:
			valuePtrs = append(valuePtrs, new([]byte)) // the value can be nil
		case intType:
			valuePtrs = append(valuePtrs, new(sql.NullInt64))
		case floatType:
			valuePtrs = append(valuePtrs, new(sql.NullFloat64))
		case datetimeType:
			valuePtrs = append(valuePtrs, new(mysql.NullTime))
		case doubleType:
			valuePtrs = append(valuePtrs, new(sql.NullFloat64))
		case timeType:
			valuePtrs = append(valuePtrs, new(sql.NullString)) // Store time as string to avoid parsing issues
		default:
			x.Panic(errors.Errorf("detected unsupported type %s on column %s",
				dataTypes[i], columns[i]))
		}
	}
	if err := rows.Scan(valuePtrs...); err != nil {
		return nil, errors.Wrapf(err, "while scanning column values")
	}
	colValues := ptrToValues(valuePtrs)
	return colValues, nil
}
