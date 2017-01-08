package schemalex

import (
	"fmt"
	"io/ioutil"

	"github.com/schemalex/schemalex/internal/errors"
	"github.com/schemalex/schemalex/statement"
	"golang.org/x/net/context"
)

// Parser is responsible to parse a set of SQL statements
type Parser struct{}

// New creates a new Parser
func New() *Parser {
	return &Parser{}
}

type parseCtx struct {
	context.Context
	input      []byte
	lexsrc     chan *Token
	peekCount  int
	peekTokens [3]*Token
}

func newParseCtx(ctx context.Context) *parseCtx {
	return &parseCtx{
		Context:   ctx,
		peekCount: -1,
	}
}

var eofToken = Token{Type: EOF}

// peek the next token. this operation fills the peekTokens
// buffer. `next()` is a combination of peek+advance.
//
// note: we do NOT check for peekCout > 2 for efficiency.
// if you do that, you're f*cked.
func (pctx *parseCtx) peek() *Token {
	if pctx.peekCount < 0 {
		select {
		case <-pctx.Context.Done():
			return &eofToken
		case t, ok := <-pctx.lexsrc:
			if !ok {
				return &eofToken
			}
			pctx.peekCount++
			pctx.peekTokens[pctx.peekCount] = t
		}
	}
	return pctx.peekTokens[pctx.peekCount]
}

func (pctx *parseCtx) advance() {
	if pctx.peekCount >= 0 {
		pctx.peekCount--
	}
}

func (pctx *parseCtx) rewind() {
	if pctx.peekCount < 2 {
		pctx.peekCount++
	}
}

func (pctx *parseCtx) next() *Token {
	t := pctx.peek()
	pctx.advance()
	return t
}

func (p *Parser) ParseFile(fn string) (Statements, error) {
	src, err := ioutil.ReadFile(fn)
	if err != nil {
		return nil, errors.Wrapf(err, `failed to open file %s`, fn)
	}

	stmts, err := p.Parse(src)
	if err != nil {
		if pe, ok := err.(*parseError); ok {
			pe.file = fn
		}
		return nil, err
	}
	return stmts, nil
}

func (p *Parser) ParseString(src string) (Statements, error) {
	return p.Parse([]byte(src))
}

// Parse parses the given set of SQL statements and creates a Statements
// structure.
// If it encounters errors while parsing, the returned error will be a
// ParseError type.
func (p *Parser) Parse(src []byte) (Statements, error) {
	cctx, cancel := context.WithCancel(context.TODO())
	defer cancel()

	ctx := newParseCtx(cctx)
	ctx.input = src
	ctx.lexsrc = Lex(cctx, src)

	var stmts []Stmt
LOOP:
	for {
		ctx.skipWhiteSpaces()
		switch t := ctx.peek(); t.Type {
		case CREATE:
			stmt, err := p.parseCreate(ctx)
			if err != nil {
				if errors.IsIgnorable(err) {
					// this is ignorable.
					continue
				}
				if pe, ok := err.(ParseError); ok {
					return nil, pe
				}
				return nil, errors.Wrap(err, `failed to parse create`)
			}
			stmts = append(stmts, stmt)
		case COMMENT_IDENT:
			ctx.advance()
		case DROP, SET, USE:
			// We don't do anything about these
		S1:
			for {
				if p.eol(ctx) {
					break S1
				}
			}
		case EOF:
			ctx.advance()
			break LOOP
		default:
			return nil, newParseError(ctx, t, "expected CREATE, COMMENT_IDENT or EOF")
		}
	}

	return stmts, nil
}

func (p *Parser) parseCreate(ctx *parseCtx) (Stmt, error) {
	if t := ctx.next(); t.Type != CREATE {
		return nil, errors.New(`expected CREATE`)
	}
	ctx.skipWhiteSpaces()
	switch t := ctx.peek(); t.Type {
	case DATABASE:
		if _, err := p.parseCreateDatabase(ctx); err != nil {
			return nil, err
		}
		return nil, errors.Ignorable(nil)
	case TABLE:
		return p.parseCreateTable(ctx)
	default:
		return nil, newParseError(ctx, t, "expected DATABASE or TABLE")
	}
}

// https://dev.mysql.com/doc/refman/5.5/en/create-database.html
// TODO: charset, collation
func (p *Parser) parseCreateDatabase(ctx *parseCtx) (statement.Database, error) {
	if t := ctx.next(); t.Type != DATABASE {
		return nil, errors.New(`expected DATABASE`)
	}

	ctx.skipWhiteSpaces()

	var notexists bool
	if ctx.peek().Type == IF {
		ctx.advance()
		if _, err := p.parseIdents(ctx, NOT, EXISTS); err != nil {
			return nil, err
		}
		notexists = true
	}

	ctx.skipWhiteSpaces()

	var database statement.Database
	switch t := ctx.next(); t.Type {
	case IDENT, BACKTICK_IDENT:
		database = statement.NewDatabase(t.Value)
	default:
		return nil, newParseError(ctx, t, "expected IDENT, BACKTICK_IDENT or IF")
	}

	database.SetIfNotExists(notexists)
	p.eol(ctx)
	return database, nil
}

// http://dev.mysql.com/doc/refman/5.6/en/create-table.html
func (p *Parser) parseCreateTable(ctx *parseCtx) (statement.Table, error) {
	if t := ctx.next(); t.Type != TABLE {
		return nil, errors.New(`expected TABLE`)
	}

	var table statement.Table

	ctx.skipWhiteSpaces()
	var temporary bool
	if t := ctx.peek(); t.Type == TEMPORARY {
		ctx.advance()
		ctx.skipWhiteSpaces()
		temporary = true
	}

	switch t := ctx.next(); t.Type {
	case IDENT, BACKTICK_IDENT:
		table = statement.NewTable(t.Value)
	default:
		return nil, newParseError(ctx, t, "expected IDENT or BACKTICK_IDENT")
	}
	table.SetTemporary(temporary)

	ctx.skipWhiteSpaces()
	if t := ctx.peek(); t.Type == IF {
		ctx.advance()
		if _, err := p.parseIdents(ctx, NOT, EXISTS); err != nil {
			return nil, newParseError(ctx, t, "should NOT EXISTS")
		}
		ctx.skipWhiteSpaces()
		table.SetIfNotExists(true)
	}

	if t := ctx.next(); t.Type != LPAREN {
		return nil, newParseError(ctx, t, "expected RPAREN")
		//		return nil, newParseError(ctx, t, "expected RPAREN")
	}

	if err := p.parseCreateTableFields(ctx, table); err != nil {
		return nil, err
	}

	return table, nil
}

// Start parsing after `CREATE TABLE *** (`
func (p *Parser) parseCreateTableFields(ctx *parseCtx, stmt statement.Table) error {
	var targetStmt interface{}

	appendStmt := func() {
		switch t := targetStmt.(type) {
		case statement.Index:
			stmt.AddIndex(t)
		case statement.TableColumn:
			stmt.AddColumn(t)
		default:
			panic(fmt.Sprintf("unexpected targetStmt: %#v", t))
		}
		targetStmt = nil
	}

	setStmt := func(t *Token, f func() (interface{}, error)) error {
		if targetStmt != nil {
			return newParseError(ctx, t, "previous column or index definition not terminated")
		}
		stmt, err := f()
		if err != nil {
			return err
		}
		targetStmt = stmt
		return nil
	}

	for {
		ctx.skipWhiteSpaces()
		switch t := ctx.next(); t.Type {
		case RPAREN:
			appendStmt()
			if err := p.parseCreateTableOptions(ctx, stmt); err != nil {
				return err
			}
			// partition option
			if !p.eol(ctx) {
				return newParseError(ctx, t, "should EOL")
			}
			return nil
		case COMMA:
			if targetStmt == nil {
				return newParseError(ctx, t, "unexpected COMMA")
			}
			appendStmt()
		case CONSTRAINT:
			err := setStmt(t, func() (interface{}, error) {
				ctx.skipWhiteSpaces()

				var sym string
				switch t := ctx.peek(); t.Type {
				case IDENT, BACKTICK_IDENT:
					// TODO: should be smarter
					// (lestrrat): I don't understand. How?
					sym = t.Value
					ctx.advance()
					ctx.skipWhiteSpaces()
				}

				var index statement.Index
				switch t := ctx.next(); t.Type {
				case PRIMARY:
					index = statement.NewIndex(statement.IndexKindPrimaryKey)
					if err := p.parseColumnIndexPrimaryKey(ctx, index); err != nil {
						return nil, err
					}
				case UNIQUE:
					index = statement.NewIndex(statement.IndexKindUnique)
					if err := p.parseColumnIndexUniqueKey(ctx, index); err != nil {
						return nil, err
					}
				case FOREIGN:
					index = statement.NewIndex(statement.IndexKindForeignKey)
					if err := p.parseColumnIndexForeignKey(ctx, index); err != nil {
						return nil, err
					}
				default:
					return nil, newParseError(ctx, t, "not supported")
				}

				if len(sym) > 0 {
					index.SetSymbol(sym)
				}
				return index, nil
			})
			if err != nil {
				return err
			}
		case PRIMARY:
			err := setStmt(t, func() (interface{}, error) {
				index := statement.NewIndex(statement.IndexKindPrimaryKey)
				if err := p.parseColumnIndexPrimaryKey(ctx, index); err != nil {
					return nil, err
				}
				return index, nil
			})
			if err != nil {
				return err
			}
		case UNIQUE:
			err := setStmt(t, func() (interface{}, error) {
				index := statement.NewIndex(statement.IndexKindUnique)
				if err := p.parseColumnIndexUniqueKey(ctx, index); err != nil {
					return nil, err
				}
				return index, nil
			})
			if err != nil {
				return err
			}
		case INDEX:
			fallthrough
		case KEY:
			err := setStmt(t, func() (interface{}, error) {
				// TODO. separate to KEY and INDEX
				index := statement.NewIndex(statement.IndexKindNormal)
				if err := p.parseColumnIndexKey(ctx, index); err != nil {
					return nil, err
				}
				return index, nil
			})
			if err != nil {
				return err
			}
		case FULLTEXT:
			err := setStmt(t, func() (interface{}, error) {
				index := statement.NewIndex(statement.IndexKindFullText)
				if err := p.parseColumnIndexFullTextKey(ctx, index); err != nil {
					return nil, err
				}
				return index, nil
			})
			if err != nil {
				return err
			}
		case SPARTIAL:
			err := setStmt(t, func() (interface{}, error) {
				index := statement.NewIndex(statement.IndexKindSpatial)
				if err := p.parseColumnIndexFullTextKey(ctx, index); err != nil {
					return nil, err
				}
				return index, nil
			})
			if err != nil {
				return err
			}
		case FOREIGN:
			err := setStmt(t, func() (interface{}, error) {
				index := statement.NewIndex(statement.IndexKindForeignKey)
				if err := p.parseColumnIndexForeignKey(ctx, index); err != nil {
					return nil, err
				}
				return index, nil
			})
			if err != nil {
				return err
			}
		case CHECK: // TODO
			return newParseError(ctx, t, "not support CHECK")
		case IDENT, BACKTICK_IDENT:

			err := setStmt(t, func() (interface{}, error) {
				col := statement.NewTableColumn(t.Value)
				if err := p.parseTableColumnSpec(ctx, col); err != nil {
					return nil, err
				}

				return col, nil
			})

			if err != nil {
				return err
			}
		default:
			return newParseError(ctx, t, "unexpected create table fields")
		}
	}
}

func (p *Parser) parseTableColumnSpec(ctx *parseCtx, col statement.TableColumn) error {
	var coltyp statement.ColumnType
	var colopt int

	ctx.skipWhiteSpaces()
	switch t := ctx.next(); t.Type {
	case BIT:
		coltyp = statement.ColumnTypeBit
		colopt = coloptSize
	case TINYINT:
		coltyp = statement.ColumnTypeTinyInt
		colopt = coloptFlagDigit
	case SMALLINT:
		coltyp = statement.ColumnTypeSmallInt
		colopt = coloptFlagDigit
	case MEDIUMINT:
		coltyp = statement.ColumnTypeMediumInt
		colopt = coloptFlagDigit
	case INT:
		coltyp = statement.ColumnTypeInt
		colopt = coloptFlagDigit
	case INTEGER:
		coltyp = statement.ColumnTypeInteger
		colopt = coloptFlagDigit
	case BIGINT:
		coltyp = statement.ColumnTypeBigInt
		colopt = coloptFlagDigit
	case REAL:
		coltyp = statement.ColumnTypeReal
		colopt = coloptFlagDecimal
	case DOUBLE:
		coltyp = statement.ColumnTypeDouble
		colopt = coloptFlagDecimal
	case FLOAT:
		coltyp = statement.ColumnTypeFloat
		colopt = coloptFlagDecimal
	case DECIMAL:
		coltyp = statement.ColumnTypeDecimal
		colopt = coloptFlagDecimalOptional
	case NUMERIC:
		coltyp = statement.ColumnTypeNumeric
		colopt = coloptFlagDecimalOptional
	case DATE:
		coltyp = statement.ColumnTypeDate
		colopt = coloptFlagNone
	case TIME:
		coltyp = statement.ColumnTypeTime
		colopt = coloptFlagTime
	case TIMESTAMP:
		coltyp = statement.ColumnTypeTimestamp
		colopt = coloptFlagTime
	case DATETIME:
		coltyp = statement.ColumnTypeDateTime
		colopt = coloptFlagTime
	case YEAR:
		coltyp = statement.ColumnTypeYear
		colopt = coloptFlagNone
	case CHAR:
		coltyp = statement.ColumnTypeChar
		colopt = coloptFlagChar
	case VARCHAR:
		coltyp = statement.ColumnTypeVarChar
		colopt = coloptFlagChar
	case BINARY:
		coltyp = statement.ColumnTypeBinary
		colopt = coloptFlagBinary
	case VARBINARY:
		coltyp = statement.ColumnTypeVarBinary
		colopt = coloptFlagBinary
	case TINYBLOB:
		coltyp = statement.ColumnTypeTinyBlob
		colopt = coloptFlagNone
	case BLOB:
		coltyp = statement.ColumnTypeBlob
		colopt = coloptFlagNone
	case MEDIUMBLOB:
		coltyp = statement.ColumnTypeMediumBlob
		colopt = coloptFlagNone
	case LONGBLOB:
		coltyp = statement.ColumnTypeLongBlob
		colopt = coloptFlagNone
	case TINYTEXT:
		coltyp = statement.ColumnTypeTinyText
		colopt = coloptFlagChar
	case TEXT:
		coltyp = statement.ColumnTypeText
		colopt = coloptFlagChar
	case MEDIUMTEXT:
		coltyp = statement.ColumnTypeMediumText
		colopt = coloptFlagChar
	case LONGTEXT:
		coltyp = statement.ColumnTypeLongText
		colopt = coloptFlagChar
	// case "ENUM":
	// case "SET":
	default:
		return newParseError(ctx, t, "not supported type")
	}

	col.SetType(coltyp)
	return p.parseColumnOption(ctx, col, colopt)
}

func (p *Parser) parseCreateTableOptions(ctx *parseCtx, stmt statement.Table) error {

	setOption := func(key string, types []TokenType) error {
		ctx.skipWhiteSpaces()
		if t := ctx.peek(); t.Type == EQUAL {
			ctx.advance()
			ctx.skipWhiteSpaces()
		}
		t := ctx.next()
		for _, typ := range types {
			if typ == t.Type {
				stmt.AddOption(statement.NewTableOption(key, t.Value))
				return nil
			}
		}
		return newParseError(ctx, t, "should %v", types)
	}

	for {
		ctx.skipWhiteSpaces()
		switch t := ctx.next(); t.Type {
		case ENGINE:
			if err := setOption("ENGINE", []TokenType{IDENT, BACKTICK_IDENT}); err != nil {
				return err
			}
		case AUTO_INCREMENT:
			if err := setOption("AUTO_INCREMENT", []TokenType{NUMBER}); err != nil {
				return err
			}
		case AVG_ROW_LENGTH:
			if err := setOption("AVG_ROW_LENGTH", []TokenType{NUMBER}); err != nil {
				return err
			}
		case DEFAULT:
			ctx.skipWhiteSpaces()
			switch t := ctx.next(); t.Type {
			case CHARACTER:
				ctx.skipWhiteSpaces()
				if t := ctx.next(); t.Type != SET {
					return newParseError(ctx, t, "expected SET")
				}
				if err := setOption("DEFAULT CHARACTER SET", []TokenType{IDENT, BACKTICK_IDENT}); err != nil {
					return err
				}
			case COLLATE:
				if err := setOption("DEFAULT COLLATE", []TokenType{IDENT, BACKTICK_IDENT}); err != nil {
					return err
				}
			default:
				return newParseError(ctx, t, "expected CHARACTER or COLLATE")
			}
		case CHARACTER:
			ctx.skipWhiteSpaces()
			if t := ctx.next(); t.Type != SET {
				return newParseError(ctx, t, "expected SET")
			}
			if err := setOption("DEFAULT CHARACTER SET", []TokenType{IDENT, BACKTICK_IDENT}); err != nil {
				return err
			}
		case COLLATE:
			if err := setOption("DEFAULT COLLATE", []TokenType{IDENT, BACKTICK_IDENT}); err != nil {
				return err
			}
		case CHECKSUM:
			if err := setOption("CHECKSUM", []TokenType{NUMBER}); err != nil {
				return err
			}
		case COMMENT:
			if err := setOption("COMMENT", []TokenType{SINGLE_QUOTE_IDENT, DOUBLE_QUOTE_IDENT}); err != nil {
				return err
			}
		case CONNECTION:
			if err := setOption("CONNECTION", []TokenType{SINGLE_QUOTE_IDENT, DOUBLE_QUOTE_IDENT}); err != nil {
				return err
			}
		case DATA:
			ctx.skipWhiteSpaces()
			if t := ctx.next(); t.Type != DIRECTORY {
				return newParseError(ctx, t, "should DIRECTORY")
			}
			if err := setOption("DATA DIRECTORY", []TokenType{SINGLE_QUOTE_IDENT, DOUBLE_QUOTE_IDENT}); err != nil {
				return err
			}
		case DELAY_KEY_WRITE:
			if err := setOption("DELAY_KEY_WRITE", []TokenType{NUMBER}); err != nil {
				return err
			}
		case INDEX:
			ctx.skipWhiteSpaces()
			if t := ctx.next(); t.Type != DIRECTORY {
				return newParseError(ctx, t, "should DIRECTORY")
			}
			if err := setOption("INDEX DIRECTORY", []TokenType{SINGLE_QUOTE_IDENT, DOUBLE_QUOTE_IDENT}); err != nil {
				return err
			}
		case INSERT_METHOD:
			if err := setOption("INSERT_METHOD", []TokenType{IDENT}); err != nil {
				return err
			}
		case KEY_BLOCK_SIZE:
			if err := setOption("KEY_BLOCK_SIZE", []TokenType{NUMBER}); err != nil {
				return err
			}
		case MAX_ROWS:
			if err := setOption("MAX_ROWS", []TokenType{NUMBER}); err != nil {
				return err
			}
		case MIN_ROWS:
			if err := setOption("MIN_ROWS", []TokenType{NUMBER}); err != nil {
				return err
			}
		case PACK_KEYS:
			if err := setOption("PACK_KEYS", []TokenType{NUMBER, IDENT}); err != nil {
				return err
			}
		case PASSWORD:
			if err := setOption("PASSWORD", []TokenType{SINGLE_QUOTE_IDENT, DOUBLE_QUOTE_IDENT}); err != nil {
				return err
			}
		case ROW_FORMAT:
			if err := setOption("ROW_FORMAT", []TokenType{DEFAULT, DYNAMIC, FIXED, COMPRESSED, REDUNDANT, COMPACT}); err != nil {
				return err
			}
		case STATS_AUTO_RECALC:
			if err := setOption("STATS_AUTO_RECALC", []TokenType{NUMBER, DEFAULT}); err != nil {
				return err
			}
		case STATS_PERSISTENT:
			if err := setOption("STATS_PERSISTENT", []TokenType{NUMBER, DEFAULT}); err != nil {
				return err
			}
		case STATS_SAMPLE_PAGES:
			if err := setOption("STATS_SAMPLE_PAGES", []TokenType{NUMBER}); err != nil {
				return err
			}
		case TABLESPACE:
			return newParseError(ctx, t, "not support TABLESPACE")
		case UNION:
			return newParseError(ctx, t, "not support UNION")
		case EOF:
			return nil
		case SEMICOLON:
			ctx.rewind()
			return nil
		default:
			return newParseError(ctx, t, "unexpected table options")
		}
	}
}

// parse for column
func (p *Parser) parseColumnOption(ctx *parseCtx, col statement.TableColumn, f int) error {
	f = f | coloptNull | coloptDefault | coloptAutoIncrement | coloptKey | coloptComment
	pos := 0
	check := func(_f int) bool {
		if pos > _f {
			return false
		}
		if f|_f != f {
			return false
		}
		pos = _f
		return true
	}
	for {
		ctx.skipWhiteSpaces()
		switch t := ctx.next(); t.Type {
		case LPAREN:
			if check(coloptSize) {
				ctx.skipWhiteSpaces()
				t := ctx.next()
				if t.Type != NUMBER {
					return newParseError(ctx, t, "expected NUMBER (column size)")
				}
				tlen := t.Value

				ctx.skipWhiteSpaces()
				t = ctx.next()
				if t.Type != RPAREN {
					return newParseError(ctx, t, "expected RPAREN (column size)")
				}
				col.SetLength(statement.NewLength(tlen))
			} else if check(coloptDecimalSize) {
				strs, err := p.parseIdents(ctx, NUMBER, COMMA, NUMBER, RPAREN)
				if err != nil {
					return err
				}
				l := statement.NewLength(strs[0])
				l.SetDecimal(strs[2])
				col.SetLength(l)
			} else if check(coloptDecimalOptionalSize) {
				ctx.skipWhiteSpaces()
				t := ctx.next()
				if t.Type != NUMBER {
					return newParseError(ctx, t, "expected NUMBER (decimal size `M`)")
				}
				tlen := t.Value

				ctx.skipWhiteSpaces()
				t = ctx.next()
				if t.Type == RPAREN {
					col.SetLength(statement.NewLength(tlen))
					continue
				} else if t.Type != COMMA {
					return newParseError(ctx, t, "expected COMMA (decimal size)")
				}

				ctx.skipWhiteSpaces()
				t = ctx.next()
				if t.Type != NUMBER {
					return newParseError(ctx, t, "expected NUMBER (decimal size `D`)")
				}
				tscale := t.Value

				ctx.skipWhiteSpaces()
				if t := ctx.next(); t.Type != RPAREN {
					return newParseError(ctx, t, "expected RPARENT (decimal size)")
				}
				l := statement.NewLength(tlen)
				l.SetDecimal(tscale)
				col.SetLength(l)
			} else {
				return newParseError(ctx, t, "cant apply coloptSize, coloptDecimalSize, coloptDecimalOptionalSize")
			}
		case UNSIGNED:
			if !check(coloptUnsigned) {
				return newParseError(ctx, t, "cant apply UNSIGNED")
			}
			col.SetUnsigned(true)
		case ZEROFILL:
			if !check(coloptZerofill) {
				return newParseError(ctx, t, "cant apply ZEROFILL")
			}
			col.SetZeroFill(true)
		case BINARY:
			if !check(coloptBinary) {
				return newParseError(ctx, t, "cant apply BINARY")
			}
			col.SetBinary(true)
		case NOT:
			if !check(coloptNull) {
				return newParseError(ctx, t, "cant apply NOT NULL")
			}
			ctx.skipWhiteSpaces()
			switch t := ctx.next(); t.Type {
			case NULL:
				col.SetNullState(statement.NullStateNotNull)
			default:
				return newParseError(ctx, t, "should NULL")
			}
		case NULL:
			if !check(coloptNull) {
				return newParseError(ctx, t, "cant apply NULL")
			}
			col.SetNullState(statement.NullStateNull)
		case DEFAULT:
			if !check(coloptDefault) {
				return newParseError(ctx, t, "cant apply DEFAULT")
			}
			ctx.skipWhiteSpaces()
			switch t := ctx.next(); t.Type {
			case IDENT, SINGLE_QUOTE_IDENT, DOUBLE_QUOTE_IDENT, NUMBER, CURRENT_TIMESTAMP, NULL:
				col.SetDefault(t.Value)
			default:
				return newParseError(ctx, t, "should IDENT, SINGLE_QUOTE_IDENT, DOUBLE_QUOTE_IDENT, NUMBER, CURRENT_TIMESTAMP, NULL")
			}
		case AUTO_INCREMENT:
			if !check(coloptAutoIncrement) {
				return newParseError(ctx, t, "cant apply AUTO_INCREMENT")
			}
			col.SetAutoIncrement(true)
		case UNIQUE:
			if !check(coloptKey) {
				return newParseError(ctx, t, "cant apply UNIQUE KEY")
			}
			ctx.skipWhiteSpaces()
			if t := ctx.next(); t.Type == KEY {
				ctx.advance()
				col.SetUnique(true)
			}
		case KEY:
			if !check(coloptKey) {
				return newParseError(ctx, t, "cant apply KEY")
			}
			col.SetKey(true)
		case PRIMARY:
			if !check(coloptKey) {
				return newParseError(ctx, t, "cant apply PRIMARY KEY")
			}
			ctx.skipWhiteSpaces()
			if t := ctx.peek(); t.Type == KEY {
				ctx.advance()
				col.SetPrimary(true)
			}
		case COMMENT:
			if !check(coloptComment) {
				return newParseError(ctx, t, "cant apply COMMENT")
			}
			ctx.skipWhiteSpaces()
			switch t := ctx.next(); t.Type {
			case SINGLE_QUOTE_IDENT:
				col.SetComment(t.Value)
			default:
				return newParseError(ctx, t, "should SINGLE_QUOTE_IDENT")
			}
		case COMMA:
			ctx.rewind()
			return nil
		case RPAREN:
			ctx.rewind()
			return nil
		default:
			return newParseError(ctx, t, "unexpected column options")
		}
	}
}

func (p *Parser) parseColumnIndexPrimaryKey(ctx *parseCtx, index statement.Index) error {
	ctx.skipWhiteSpaces()
	if t := ctx.next(); t.Type != KEY {
		return newParseError(ctx, t, "should KEY")
	}
	if err := p.parseColumnIndexType(ctx, index); err != nil {
		return err
	}

	if err := p.parseColumnIndexColName(ctx, index); err != nil {
		return err
	}

	return nil
}

func (p *Parser) parseColumnIndexUniqueKey(ctx *parseCtx, index statement.Index) error {
	ctx.skipWhiteSpaces()
	switch t := ctx.peek(); t.Type {
	case KEY, INDEX:
		ctx.advance()
	}

	if err := p.parseColumnIndexName(ctx, index); err != nil {
		return err
	}
	if err := p.parseColumnIndexType(ctx, index); err != nil {
		return err
	}

	if err := p.parseColumnIndexColName(ctx, index); err != nil {
		return err
	}

	return nil
}

func (p *Parser) parseColumnIndexKey(ctx *parseCtx, index statement.Index) error {
	if err := p.parseColumnIndexName(ctx, index); err != nil {
		return err
	}
	if err := p.parseColumnIndexType(ctx, index); err != nil {
		return err
	}

	if err := p.parseColumnIndexColName(ctx, index); err != nil {
		return err
	}

	return nil
}

func (p *Parser) parseColumnIndexFullTextKey(ctx *parseCtx, index statement.Index) error {
	if err := p.parseColumnIndexName(ctx, index); err != nil {
		return err
	}

	if err := p.parseColumnIndexColName(ctx, index); err != nil {
		return err
	}

	return nil
}

func (p *Parser) parseColumnIndexForeignKey(ctx *parseCtx, index statement.Index) error {
	ctx.skipWhiteSpaces()
	if t := ctx.next(); t.Type != KEY {
		return newParseError(ctx, t, "should KEY")
	}
	if err := p.parseColumnIndexName(ctx, index); err != nil {
		return err
	}

	if err := p.parseColumnIndexColName(ctx, index); err != nil {
		return err
	}

	ctx.skipWhiteSpaces()
	if t := ctx.peek(); t.Type == REFERENCES {
		if err := p.parseColumnReference(ctx, index); err != nil {
			return err
		}
	}

	return nil
}

func (p *Parser) parseReferenceOption(ctx *parseCtx, set func(statement.ReferenceOption)) error {
	ctx.skipWhiteSpaces()
	switch t := ctx.next(); t.Type {
	case RESTRICT:
		set(statement.ReferenceOptionRestrict)
	case CASCADE:
		set(statement.ReferenceOptionCascade)
	case SET:
		ctx.skipWhiteSpaces()
		if t := ctx.next(); t.Type != NULL {
			return newParseError(ctx, t, "expected NULL")
		}
		set(statement.ReferenceOptionSetNull)
	case NO:
		ctx.skipWhiteSpaces()
		if t := ctx.next(); t.Type != ACTION {
			return newParseError(ctx, t, "expected ACTION")
		}
		set(statement.ReferenceOptionNoAction)
	default:
		return newParseError(ctx, t, "expected RESTRICT, CASCADE, SET or NO")
	}
	return nil
}

func (p *Parser) parseColumnReference(ctx *parseCtx, index statement.Index) error {
	ctx.skipWhiteSpaces()
	if t := ctx.next(); t.Type != REFERENCES {
		return newParseError(ctx, t, "expected REFERENCES")
	}

	r := statement.NewReference()

	ctx.skipWhiteSpaces()
	switch t := ctx.next(); t.Type {
	case BACKTICK_IDENT, IDENT:
		r.SetTableName(t.Value)
	default:
		return newParseError(ctx, t, "should IDENT or BACKTICK_IDENT")
	}

	if err := p.parseColumnIndexColName(ctx, r); err != nil {
		return err
	}

	ctx.skipWhiteSpaces()
	if t := ctx.peek(); t.Type == MATCH {
		ctx.advance()
		ctx.skipWhiteSpaces()
		switch t = ctx.next(); t.Type {
		case FULL:
			r.SetMatch(statement.ReferenceMatchFull)
		case PARTIAL:
			r.SetMatch(statement.ReferenceMatchPartial)
		case SIMPLE:
			r.SetMatch(statement.ReferenceMatchSimple)
		default:
			return newParseError(ctx, t, "should FULL, PARTIAL or SIMPLE")
		}
		ctx.skipWhiteSpaces()
	}

	// ON DELETE can be followed by ON UPDATE, but
	// ON UPDATE cannot be followed by ON DELETE
OUTER:
	for i := 0; i < 2; i++ {
		ctx.skipWhiteSpaces()
		if t := ctx.peek(); t.Type != ON {
			break OUTER
		}
		ctx.advance()
		ctx.skipWhiteSpaces()

		switch t := ctx.next(); t.Type {
		case DELETE:
			if err := p.parseReferenceOption(ctx, r.SetOnDelete); err != nil {
				return errors.Wrap(err, `failed to parse ON DELETE`)
			}
		case UPDATE:
			if err := p.parseReferenceOption(ctx, r.SetOnUpdate); err != nil {
				return errors.Wrap(err, `failed to parse ON UPDATE`)
			}
			break OUTER
		default:
			return newParseError(ctx, t, "expected DELETE or UPDATE")
		}
	}

	index.SetReference(r)
	return nil
}

func (p *Parser) parseColumnIndexName(ctx *parseCtx, index statement.Index) error {
	ctx.skipWhiteSpaces()
	switch t := ctx.peek(); t.Type {
	case BACKTICK_IDENT, IDENT:
		ctx.advance()
		index.SetName(t.Value)
	}
	return nil
}

func (p *Parser) parseColumnIndexTypeUsing(ctx *parseCtx, index statement.Index) error {
	if t := ctx.next(); t.Type != USING {
		return errors.New(`expected USING`)
	}

	ctx.skipWhiteSpaces()
	switch t := ctx.next(); t.Type {
	case BTREE:
		index.SetType(statement.IndexTypeBtree)
	case HASH:
		index.SetType(statement.IndexTypeHash)
	default:
		return newParseError(ctx, t, "should BTREE or HASH")
	}
	return nil
}

func (p *Parser) parseColumnIndexType(ctx *parseCtx, index statement.Index) error {
	ctx.skipWhiteSpaces()
	if t := ctx.peek(); t.Type == USING {
		return p.parseColumnIndexTypeUsing(ctx, index)
	}

	index.SetType(statement.IndexTypeNone)
	return nil
}

// TODO rename method name
func (p *Parser) parseColumnIndexColName(ctx *parseCtx, container interface {
	AddColumns(...string)
}) error {
	var cols []string

	ctx.skipWhiteSpaces()
	if t := ctx.next(); t.Type != LPAREN {
		return newParseError(ctx, t, "should (")
	}

OUTER:
	for {
		ctx.skipWhiteSpaces()
		t := ctx.next()
		if !(t.Type == IDENT || t.Type == BACKTICK_IDENT) {
			return newParseError(ctx, t, "should IDENT or BACKTICK_IDENT")
		}
		cols = append(cols, t.Value)

		ctx.skipWhiteSpaces()
		switch t = ctx.next(); t.Type {
		case COMMA:
			// search next
			continue
		case RPAREN:
			break OUTER
		default:
			return newParseError(ctx, t, "should , or )")
		}
	}

	container.AddColumns(cols...)
	return nil
}

// Skips over whitespaces. Once this method returns, you can be
// certain that next call to ctx.next()/peek() will result in a
// non-space token
func (ctx *parseCtx) skipWhiteSpaces() {
	for {
		switch t := ctx.peek(); t.Type {
		case SPACE, COMMENT_IDENT:
			ctx.advance()
			continue
		default:
			return
		}
	}
}

func (p *Parser) parseIdents(ctx *parseCtx, idents ...TokenType) ([]string, error) {
	strs := []string{}
	for _, ident := range idents {
		ctx.skipWhiteSpaces()
		t := ctx.next()
		if t.Type != ident {
			return nil, newParseError(ctx, t, "expected %v", idents)
		}
		strs = append(strs, t.Value)
	}
	return strs, nil
}

func (p *Parser) eol(ctx *parseCtx) bool {
	ctx.skipWhiteSpaces()
	switch t := ctx.peek(); t.Type {
	case EOF, SEMICOLON:
		ctx.advance()
		return true
	default:
		return false
	}
}
