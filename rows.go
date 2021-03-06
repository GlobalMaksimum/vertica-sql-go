package vertigo

// Copyright (c) 2019-2020 Micro Focus or one of its affiliates.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/GlobalMaksimum/vertica-sql-go/common"
	"github.com/GlobalMaksimum/vertica-sql-go/logger"
	"github.com/GlobalMaksimum/vertica-sql-go/msgs"
	"github.com/GlobalMaksimum/vertica-sql-go/rowcache"
)

type rowStore interface {
	AddRow(msg *msgs.BEDataRowMsg)
	GetRow() *msgs.BEDataRowMsg
	Peek() *msgs.BEDataRowMsg
	Close() error
	Finalize() error
}

type rows struct {
	columnDefs    *msgs.BERowDescMsg
	goColumnTypes []*sql.ColumnType
	resultData    rowStore

	tzOffset      string
	inMemRowLimit int
}

var (
	paddingString        = "000000"
	defaultRowBufferSize = 256
	rowLogger            = logger.New("row")
)

// Columns returns the names of all of the columns
// Interface: driver.Rows
func (r *rows) Columns() []string {
	columnLabels := make([]string, len(r.columnDefs.Columns))
	for idx, cd := range r.columnDefs.Columns {
		columnLabels[idx] = cd.FieldName
	}
	return columnLabels
}

// Close closes the read cursor
// Interface: driver.Rows
func (r *rows) Close() error {
	return r.resultData.Close()
}

func (r *rows) Next(dest []driver.Value) error {
	var err error
	nextRow := r.resultData.GetRow()
	if nextRow == nil {
		return io.EOF
	}

	rowCols := nextRow.Columns()

	for idx := uint16(0); idx < rowCols.NumCols; idx++ {
		colVal := rowCols.Chunk()
		if colVal == nil {
			dest[idx] = nil
			continue
		}

		switch r.columnDefs.Columns[idx].DataTypeOID {
		case common.ColTypeBoolean: // to boolean
			dest[idx] = colVal[0] == 't'
		case common.ColTypeInt64: // to integer
			dest[idx], err = strconv.Atoi(string(colVal))
		case common.ColTypeVarChar, common.ColTypeLongVarChar, common.ColTypeChar, common.ColTypeUUID: // stays string, convert char to string
			dest[idx] = string(colVal)
		case common.ColTypeFloat64, common.ColTypeNumeric: // to float64
			dest[idx], err = strconv.ParseFloat(string(colVal), 64)
		case common.ColTypeTimestamp: // to time.Time from YYYY-MM-DD hh:mm:ss
			dest[idx], err = parseTimestampTZColumn(string(colVal) + r.tzOffset)
		case common.ColTypeTimestampTZ:
			dest[idx], err = parseTimestampTZColumn(string(colVal))
		case common.ColTypeVarBinary, common.ColTypeLongVarBinary, common.ColTypeBinary: // to []byte - this one's easy
			dest[idx] = hex.EncodeToString(colVal)
		default:
			dest[idx] = string(colVal)
		}

		if err != nil {
			rowLogger.Error("%s", err.Error())
		}
	}

	return err
}

func parseTimestampTZColumn(fullString string) (driver.Value, error) {
	var result driver.Value
	var err error

	endsWithHalfHour, _ := regexp.Compile(".*:\\d{2}$")
	if !endsWithHalfHour.MatchString(fullString) {
		fullString = fullString + ":00"
	}

	// ensures ms are included with the desired length
	if strings.IndexByte(fullString, '.') == 19 {
		neededPadding := 32 - len(fullString)
		if neededPadding > 0 {
			fullString = fullString[0:26-neededPadding] + paddingString[0:neededPadding] + fullString[26-neededPadding:]
		}
	} else {
		fullString = fullString[0:19] + "." + paddingString[0:6] + fullString[19:]
	}

	result, err = time.Parse("2006-01-02 15:04:05.000000-07:00", fullString)

	return result, err
}

func (r *rows) finalize() {
	r.resultData.Finalize()
}

func (r *rows) addRow(rowData *msgs.BEDataRowMsg) {
	r.resultData.AddRow(rowData)
}

func newRows(ctx context.Context, columnsDefsMsg *msgs.BERowDescMsg, tzOffset string) *rows {

	rowBufferSize := defaultRowBufferSize
	inMemRowLimit := 0
	var resultData rowStore
	var err error

	if vCtx, ok := ctx.(VerticaContext); ok {
		rowBufferSize = vCtx.GetInMemoryResultRowLimit()
		inMemRowLimit = rowBufferSize
	}
	if inMemRowLimit != 0 {
		resultData, err = rowcache.NewFileCache(inMemRowLimit)
		if err != nil {
			resultData = rowcache.NewMemoryCache(rowBufferSize)
		}
	} else {
		resultData = rowcache.NewMemoryCache(rowBufferSize)
	}

	res := &rows{
		columnDefs:    columnsDefsMsg,
		resultData:    resultData,
		tzOffset:      tzOffset,
		inMemRowLimit: inMemRowLimit,
	}

	return res
}

func (r *rows) ColumnTypeDatabaseTypeName(index int) string {
	return r.columnDefs.Columns[index].DataTypeName
}

func (r *rows) ColumnTypeLength(index int) (length int64, ok bool) {
	return int64(r.columnDefs.Columns[index].Length), true
}

func (r *rows) ColumnTypeNullable(index int) (isNullable bool, ok bool) {
	return r.columnDefs.Columns[index].Nullable, r.columnDefs.Columns[index].Nullable
}

//func (r *rows) ColumnTypePrecisionScale(index int) (precision, scale int64, ok bool) {
//	return r.columnDefs.Columns[index].
//}

func newEmptyRows() *rows {
	cdf := make([]*msgs.BERowDescColumnDef, 0)
	be := &msgs.BERowDescMsg{Columns: cdf}
	return newRows(context.Background(), be, "")
}
