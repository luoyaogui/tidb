// Copyright 2016 PingCAP, Inc.
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

package privileges

import (
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/juju/errors"
	"github.com/pingcap/tidb/ast"
	"github.com/pingcap/tidb/context"
	"github.com/pingcap/tidb/mysql"
	"github.com/pingcap/tidb/util/sqlexec"
	"github.com/pingcap/tidb/util/types"
)

const (
	userTablePrivilegeMask = mysql.SelectPriv | mysql.InsertPriv | mysql.UpdatePriv | mysql.DeletePriv | mysql.CreatePriv | mysql.DropPriv | mysql.GrantPriv | mysql.IndexPriv | mysql.AlterPriv | mysql.ShowDBPriv | mysql.ExecutePriv | mysql.CreateUserPriv
	dbTablePrivilegeMask   = mysql.SelectPriv | mysql.InsertPriv | mysql.UpdatePriv | mysql.DeletePriv | mysql.CreatePriv | mysql.DropPriv | mysql.GrantPriv | mysql.IndexPriv | mysql.AlterPriv
	tablePrivMask          = mysql.SelectPriv | mysql.InsertPriv | mysql.UpdatePriv | mysql.DeletePriv | mysql.CreatePriv | mysql.DropPriv | mysql.GrantPriv | mysql.IndexPriv | mysql.AlterPriv
	columnPrivMask         = mysql.SelectPriv | mysql.InsertPriv | mysql.UpdatePriv
)

type userRecord struct {
	Host       string // max length 60, primary key
	User       string // max length 16, primary key
	Password   string // max length 41
	Privileges mysql.PrivilegeType
}

type dbRecord struct {
	Host       string
	DB         string
	User       string
	Privileges mysql.PrivilegeType
}

type tablesPrivRecord struct {
	Host       string
	DB         string
	User       string
	TableName  string
	Grantor    string
	Timestamp  time.Time
	TablePriv  mysql.PrivilegeType
	ColumnPriv mysql.PrivilegeType
}

type columnsPrivRecord struct {
	Host       string
	DB         string
	User       string
	TableName  string
	ColumnName string
	Timestamp  time.Time
	ColumnPriv mysql.PrivilegeType
}

// MySQLPrivilege is the in-memory cache of mysql privilege tables.
type MySQLPrivilege struct {
	User        []userRecord
	DB          []dbRecord
	TablesPriv  []tablesPrivRecord
	ColumnsPriv []columnsPrivRecord
}

// LoadAll loads the tables from database to memory.
func (p *MySQLPrivilege) LoadAll(ctx context.Context) error {
	err := p.LoadUserTable(ctx)
	if err != nil {
		return errors.Trace(err)
	}
	err = p.LoadDBTable(ctx)
	if err != nil {
		return errors.Trace(err)
	}
	err = p.LoadTablesPrivTable(ctx)
	if err != nil {
		return errors.Trace(err)
	}
	err = p.LoadColumnsPrivTable(ctx)
	if err != nil {
		return errors.Trace(err)
	}
	return nil
}

// LoadUserTable loads the mysql.user table from database.
func (p *MySQLPrivilege) LoadUserTable(ctx context.Context) error {
	return p.loadTable(ctx, "select * from mysql.user order by host, user;", p.decodeUserTableRow)
}

// LoadDBTable loads the mysql.db table from database.
func (p *MySQLPrivilege) LoadDBTable(ctx context.Context) error {
	return p.loadTable(ctx, "select * from mysql.db order by host, db, user;", p.decodeDBTableRow)
}

// LoadTablesPrivTable loads the mysql.tables_priv table from database.
func (p *MySQLPrivilege) LoadTablesPrivTable(ctx context.Context) error {
	return p.loadTable(ctx, "select * from mysql.tables_priv", p.decodeTablesPrivTableRow)
}

// LoadColumnsPrivTable loads the mysql.columns_priv table from database.
func (p *MySQLPrivilege) LoadColumnsPrivTable(ctx context.Context) error {
	return p.loadTable(ctx, "select * from mysql.columns_priv", p.decodeColumnsPrivTableRow)
}

func (p *MySQLPrivilege) loadTable(ctx context.Context, sql string,
	decodeTableRow func(*ast.Row, []*ast.ResultField) error) error {
	rs, err := ctx.(sqlexec.RestrictedSQLExecutor).ExecRestrictedSQL(ctx, sql)
	if err != nil {
		return errors.Trace(err)
	}
	defer rs.Close()

	fs, err := rs.Fields()
	if err != nil {
		return errors.Trace(err)
	}
	for {
		row, err := rs.Next()
		if err != nil {
			return errors.Trace(err)
		}
		if row == nil {
			break
		}

		err = decodeTableRow(row, fs)
		if err != nil {
			return errors.Trace(err)
		}
	}
	return nil
}

func (p *MySQLPrivilege) decodeUserTableRow(row *ast.Row, fs []*ast.ResultField) error {
	var value userRecord
	for i, f := range fs {
		d := row.Data[i]
		switch {
		case f.ColumnAsName.L == "user":
			value.User = d.GetString()
		case f.ColumnAsName.L == "host":
			value.Host = d.GetString()
		case f.ColumnAsName.L == "password":
			value.Password = d.GetString()
		case d.Kind() == types.KindMysqlEnum:
			ed := d.GetMysqlEnum()
			if ed.String() != "Y" {
				continue
			}
			priv, ok := mysql.Col2PrivType[f.ColumnAsName.O]
			if !ok {
				return errInvalidPrivilegeType.Gen("Unknown Privilege Type!")
			}
			value.Privileges |= priv
		}
	}
	p.User = append(p.User, value)
	return nil
}

func (p *MySQLPrivilege) decodeDBTableRow(row *ast.Row, fs []*ast.ResultField) error {
	var value dbRecord
	for i, f := range fs {
		d := row.Data[i]
		switch {
		case f.ColumnAsName.L == "user":
			value.User = d.GetString()
		case f.ColumnAsName.L == "host":
			value.Host = d.GetString()
		case f.ColumnAsName.L == "db":
			value.DB = d.GetString()
		case d.Kind() == types.KindMysqlEnum:
			ed := d.GetMysqlEnum()
			if ed.String() != "Y" {
				continue
			}
			priv, ok := mysql.Col2PrivType[f.ColumnAsName.O]
			if !ok {
				return errInvalidPrivilegeType.Gen("Unknown Privilege Type!")
			}
			value.Privileges |= priv
		}
	}
	p.DB = append(p.DB, value)
	return nil
}

func (p *MySQLPrivilege) decodeTablesPrivTableRow(row *ast.Row, fs []*ast.ResultField) error {
	var value tablesPrivRecord
	for i, f := range fs {
		d := row.Data[i]
		switch {
		case f.ColumnAsName.L == "user":
			value.User = d.GetString()
		case f.ColumnAsName.L == "host":
			value.Host = d.GetString()
		case f.ColumnAsName.L == "db":
			value.DB = d.GetString()
		case f.ColumnAsName.L == "table_name":
			value.TableName = d.GetString()
		case f.ColumnAsName.L == "table_priv":
			priv, err := decodeSetToPrivilege(d.GetMysqlSet())
			if err != nil {
				return errors.Trace(err)
			}
			value.TablePriv = priv
		case f.ColumnAsName.L == "column_priv":
			priv, err := decodeSetToPrivilege(d.GetMysqlSet())
			if err != nil {
				return errors.Trace(err)
			}
			value.ColumnPriv = priv
		}
	}
	p.TablesPriv = append(p.TablesPriv, value)
	return nil
}

func (p *MySQLPrivilege) decodeColumnsPrivTableRow(row *ast.Row, fs []*ast.ResultField) error {
	var value columnsPrivRecord
	for i, f := range fs {
		d := row.Data[i]
		switch {
		case f.ColumnAsName.L == "user":
			value.User = d.GetString()
		case f.ColumnAsName.L == "host":
			value.Host = d.GetString()
		case f.ColumnAsName.L == "db":
			value.DB = d.GetString()
		case f.ColumnAsName.L == "table_name":
			value.TableName = d.GetString()
		case f.ColumnAsName.L == "column_name":
			value.ColumnName = d.GetString()
		case f.ColumnAsName.L == "timestamp":
			value.Timestamp, _ = d.GetMysqlTime().Time.GoTime(time.Local)
		case f.ColumnAsName.L == "column_priv":
			priv, err := decodeSetToPrivilege(d.GetMysqlSet())
			if err != nil {
				return errors.Trace(err)
			}
			value.ColumnPriv = priv
		}
	}
	p.ColumnsPriv = append(p.ColumnsPriv, value)
	return nil
}

func decodeSetToPrivilege(s types.Set) (mysql.PrivilegeType, error) {
	var ret mysql.PrivilegeType
	if s.Name == "" {
		return ret, nil
	}
	for _, str := range strings.Split(s.Name, ",") {
		priv, ok := mysql.SetStr2Priv[str]
		if !ok {
			return ret, errInvalidPrivilegeType
		}
		ret |= priv
	}
	return ret, nil
}

func (record *userRecord) match(user, host string) bool {
	return record.User == user && patternMatch(record.Host, host)
}

func (record *dbRecord) match(user, host, db string) bool {
	return record.User == user && patternMatch(record.Host, host) && record.DB == db
}

func (record *tablesPrivRecord) match(user, host, db, table string) bool {
	return record.User == user && patternMatch(record.Host, host) &&
		record.DB == db && record.TableName == table
}

func (record *columnsPrivRecord) match(user, host, db, table, col string) bool {
	return record.User == user && patternMatch(record.Host, host) &&
		record.DB == db && record.TableName == table && record.ColumnName == col
}

func patternMatch(pattern, str string) bool {
	for i := 0; i < len(pattern); i++ {
		p := pattern[i]
		if p == '%' {
			return true
		}
		if i >= len(str) || p != str[i] {
			return false
		}
	}
	return len(pattern) == len(str)
}

// ConnectionVerification verifies the connection have access to TiDB server.
func (p *MySQLPrivilege) ConnectionVerification(user, host string) bool {
	for _, record := range p.User {
		if record.match(user, host) {
			return true
		}
	}
	return false
}

// RequestVerification checks whether ther userhave sufficient privileges to do the operation.
func (p *MySQLPrivilege) RequestVerification(user, host, db, table, column string, priv mysql.PrivilegeType) bool {
	for _, record := range p.User {
		if record.match(user, host) && record.Privileges&priv > 0 {
			return true
		}
	}

	for _, record := range p.DB {
		if record.match(user, host, db) && record.Privileges&priv > 0 {
			return true
		}
	}

	for _, record := range p.TablesPriv {
		if record.match(user, host, db, table) {
			if record.TablePriv&priv > 0 {
				return true
			}
			if column != "" && record.ColumnPriv&priv > 0 {
				return true
			}
		}
	}

	for _, record := range p.ColumnsPriv {
		if record.match(user, host, db, table, column) && record.ColumnPriv&priv > 0 {
			return true
		}
	}

	return false
}

// Handle wraps MySQLPrivilege providing thread safe access.
type Handle struct {
	priv *MySQLPrivilege
}

// Get the MySQLPrivilege for read.
func (h *Handle) Get() *MySQLPrivilege {
	addr := (*uintptr)(unsafe.Pointer(&h.priv))
	ptr := atomic.LoadUintptr(addr)
	return (*MySQLPrivilege)(unsafe.Pointer(ptr))
}

// Update the MySQLPrivilege.
func (h *Handle) Update(ctx context.Context) error {
	var priv MySQLPrivilege
	err := priv.LoadAll(ctx)
	if err != nil {
		return errors.Trace(err)
	}

	addr := (*uintptr)(unsafe.Pointer(&h.priv))
	ptr := (uintptr)(unsafe.Pointer(&priv))
	atomic.StoreUintptr(addr, ptr)
	return nil
}
