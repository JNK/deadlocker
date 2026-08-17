package wire

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Capability flags we care about.
const (
	capProtocol41      = 0x00000200
	capSSL             = 0x00000800
	capTransactions    = 0x00002000
	capSecureConn      = 0x00008000
	capMultiStatements = 0x00010000
	capMultiResults    = 0x00020000
	capPluginAuth      = 0x00080000
	capConnectAttrs    = 0x00100000
	capDeprecateEOF    = 0x01000000
	capQueryAttributes = 0x08000000
	capCompress        = 0x00000020
	capZstdCompression = 0x04000000
)

// Server status flags carried on every OK/EOF packet. IN_TRANS is the one that
// makes the UI useful: it shows exactly when a connection opened or released
// its transaction.
const (
	statusInTrans          = 0x0001
	statusAutocommit       = 0x0002
	statusMoreResults      = 0x0008
	statusNoGoodIndexUsed  = 0x0010
	statusNoIndexUsed      = 0x0020
	statusCursorExists     = 0x0040
	statusLastRowSent      = 0x0080
	statusDBDropped        = 0x0100
	statusNoBackslashEsc   = 0x0200
	statusMetadataChanged  = 0x0400
	statusQueryWasSlow     = 0x0800
	statusPSOutParams      = 0x1000
	statusInTransReadonly  = 0x2000
	statusSessionStateChgd = 0x4000
)

func statusFlagNames(f uint16) []string {
	type fl struct {
		bit  uint16
		name string
	}
	all := []fl{
		{statusInTrans, "IN_TRANS"},
		{statusAutocommit, "AUTOCOMMIT"},
		{statusMoreResults, "MORE_RESULTS_EXISTS"},
		{statusNoGoodIndexUsed, "NO_GOOD_INDEX_USED"},
		{statusNoIndexUsed, "NO_INDEX_USED"},
		{statusCursorExists, "CURSOR_EXISTS"},
		{statusLastRowSent, "LAST_ROW_SENT"},
		{statusDBDropped, "DB_DROPPED"},
		{statusNoBackslashEsc, "NO_BACKSLASH_ESCAPES"},
		{statusMetadataChanged, "METADATA_CHANGED"},
		{statusQueryWasSlow, "QUERY_WAS_SLOW"},
		{statusPSOutParams, "PS_OUT_PARAMS"},
		{statusInTransReadonly, "IN_TRANS_READONLY"},
		{statusSessionStateChgd, "SESSION_STATE_CHANGED"},
	}
	var out []string
	for _, f2 := range all {
		if f&f2.bit != 0 {
			out = append(out, f2.name)
		}
	}
	return out
}

var commandNames = map[byte]string{
	0x00: "COM_SLEEP", 0x01: "COM_QUIT", 0x02: "COM_INIT_DB", 0x03: "COM_QUERY",
	0x04: "COM_FIELD_LIST", 0x05: "COM_CREATE_DB", 0x06: "COM_DROP_DB",
	0x07: "COM_REFRESH", 0x08: "COM_SHUTDOWN", 0x09: "COM_STATISTICS",
	0x0a: "COM_PROCESS_INFO", 0x0c: "COM_PROCESS_KILL", 0x0d: "COM_DEBUG",
	0x0e: "COM_PING", 0x11: "COM_CHANGE_USER", 0x16: "COM_STMT_PREPARE",
	0x17: "COM_STMT_EXECUTE", 0x18: "COM_STMT_SEND_LONG_DATA",
	0x19: "COM_STMT_CLOSE", 0x1a: "COM_STMT_RESET", 0x1b: "COM_SET_OPTION",
	0x1c: "COM_STMT_FETCH", 0x1f: "COM_RESET_CONNECTION",
}

// Event is one decoded packet, ready to render.
type Event struct {
	ConnID    string    `json:"conn_id"`
	Actor     string    `json:"actor"`
	Index     int       `json:"index"` // packet ordinal within the connection
	Direction Direction `json:"direction"`
	At        time.Time `json:"at"`
	Seq       uint8     `json:"seq"`
	Bytes     int       `json:"bytes"`

	Phase   string `json:"phase"`   // handshake | command
	Kind    string `json:"kind"`    // COM_QUERY, OK, ERR, Row, ColumnDefinition, …
	Summary string `json:"summary"` // one-line human description

	// Populated depending on Kind.
	Query        string   `json:"query,omitempty"`
	ErrCode      uint16   `json:"err_code,omitempty"`
	SQLState     string   `json:"sql_state,omitempty"`
	ErrMessage   string   `json:"err_message,omitempty"`
	AffectedRows uint64   `json:"affected_rows,omitempty"`
	LastInsertID uint64   `json:"last_insert_id,omitempty"`
	Warnings     uint16   `json:"warnings,omitempty"`
	StatusFlags  []string `json:"status_flags,omitempty"`
	Columns      []string `json:"columns,omitempty"`
	Values       []string `json:"values,omitempty"`
	Info         string   `json:"info,omitempty"`
	// Protocol distinguishes text rows (COM_QUERY) from binary rows
	// (COM_STMT_EXECUTE), which are encoded completely differently.
	Protocol string `json:"protocol,omitempty"`

	Hex       string `json:"hex"`
	DecodeErr string `json:"decode_err,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

type respState int

const (
	respNone respState = iota
	respFirst
	respPrepareParams
	respPrepareColumns
	respColumnDefs
	respRows
)

// connDecoder holds the protocol state for one client connection. The two
// forwarding goroutines (client→server and server→client) both call into it,
// so every entry point takes the mutex; the request/response protocol keeps
// them naturally serialised anyway.
type connDecoder struct {
	mu sync.Mutex

	connID string
	actor  string

	index       int
	inHandshake bool
	caps        uint32

	state          respState
	lastCommand    byte
	lastQuery      string
	columnsWanted  int
	columnsSeen    int
	paramsWanted   int
	paramsSeen     int
	columns        []columnDef
	rowCount       int
	awaitColumnEOF bool

	// stmts maps a prepared statement id to its SQL, so COM_STMT_EXECUTE can
	// be shown as something more useful than an opaque integer.
	stmts       map[uint32]string
	pendingPrep string
}

func newConnDecoder(connID, actor string) *connDecoder {
	return &connDecoder{
		connID:      connID,
		actor:       actor,
		inHandshake: true,
		state:       respNone,
		stmts:       map[uint32]string{},
	}
}

const hexDumpLimit = 512

func hexDump(b []byte) (string, bool) {
	truncated := false
	if len(b) > hexDumpLimit {
		b = b[:hexDumpLimit]
		truncated = true
	}
	return hex.EncodeToString(b), truncated
}

// decode turns a forwarded packet into an Event.
func (d *connDecoder) decode(dir Direction, p *Packet, at time.Time) Event {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.index++
	ev := Event{
		ConnID:    d.connID,
		Actor:     d.actor,
		Index:     d.index,
		Direction: dir,
		At:        at,
		Seq:       p.Seq,
		Bytes:     len(p.Raw),
	}
	ev.Hex, ev.Truncated = hexDump(p.Raw)

	if d.inHandshake {
		ev.Phase = "handshake"
		d.decodeHandshake(dir, p, &ev)
		return ev
	}
	ev.Phase = "command"
	if dir == ClientToServer {
		d.decodeCommand(p, &ev)
	} else {
		d.decodeResponse(p, &ev)
	}
	return ev
}

// decodeHandshake labels the connection-setup packets. Authentication payloads
// are deliberately not interpreted beyond their type: with
// caching_sha2_password the interesting bytes are an RSA-encrypted password and
// decoding them would be neither possible nor useful.
func (d *connDecoder) decodeHandshake(dir Direction, p *Packet, ev *Event) {
	r := newReader(p.Payload)

	if dir == ServerToClient {
		if len(p.Payload) == 0 {
			ev.Kind, ev.Summary = "Unknown", "empty packet"
			return
		}
		switch p.Payload[0] {
		case 0xFF:
			d.decodeErrPacket(r, ev)
			d.inHandshake = false
			return
		case 0x00:
			// OK: handshake finished, command phase begins.
			d.decodeOKPacket(r, ev, false)
			ev.Kind = "OK"
			ev.Summary = "authentication succeeded — entering command phase"
			d.inHandshake = false
			d.state = respNone
			return
		case 0x01:
			// AuthMoreData: caching_sha2_password fast/full auth signalling.
			r.byte()
			ev.Kind = "AuthMoreData"
			switch {
			case r.remaining() == 1 && p.Payload[1] == 3:
				ev.Summary = "caching_sha2_password: fast auth succeeded"
			case r.remaining() == 1 && p.Payload[1] == 4:
				ev.Summary = "caching_sha2_password: full authentication required"
			default:
				ev.Summary = fmt.Sprintf("server auth data (%d bytes) — RSA public key or scramble", r.remaining())
			}
			return
		case 0xFE:
			r.byte()
			plugin := r.nulString()
			ev.Kind = "AuthSwitchRequest"
			ev.Summary = "server requests auth plugin " + plugin
			return
		case 0x0A:
			d.decodeServerGreeting(r, ev)
			return
		}
		ev.Kind, ev.Summary = "HandshakeData", fmt.Sprintf("server handshake payload (%d bytes)", len(p.Payload))
		return
	}

	// Client side. The first client packet in the handshake is the response;
	// anything after it is auth continuation.
	if p.Seq == 1 && len(p.Payload) >= 32 {
		d.decodeHandshakeResponse(r, ev)
		return
	}
	ev.Kind = "AuthData"
	switch {
	case len(p.Payload) == 1 && p.Payload[0] == 2:
		ev.Summary = "client requests the server's RSA public key"
	case len(p.Payload) == 0:
		ev.Summary = "empty auth response (no password)"
	default:
		ev.Summary = fmt.Sprintf("client auth data (%d bytes) — scrambled or RSA-encrypted password", len(p.Payload))
	}
}

func (d *connDecoder) decodeServerGreeting(r *reader, ev *Event) {
	r.byte() // protocol version 10
	version := r.nulString()
	threadID := r.uint32()
	r.skip(8) // auth-plugin-data-part-1
	r.skip(1) // filler
	capLower := uint32(r.uint16())
	r.skip(1) // charset
	statusFlags := r.uint16()
	capUpper := uint32(r.uint16())
	caps := capLower | capUpper<<16

	ev.Kind = "ServerGreeting"
	ev.StatusFlags = statusFlagNames(statusFlags)
	ev.Summary = fmt.Sprintf("MySQL %s, connection id %d", version, threadID)
	if caps&capSSL != 0 {
		ev.Info = "server offers TLS (client declines: the proxy needs plaintext to decode)"
	}
	if r.err != nil {
		ev.DecodeErr = r.err.Error()
	}
}

func (d *connDecoder) decodeHandshakeResponse(r *reader, ev *Event) {
	caps := r.uint32()
	d.caps = caps
	maxPacket := r.uint32()
	charset := r.byte()
	r.skip(23)
	user := r.nulString()

	ev.Kind = "HandshakeResponse"
	var notable []string
	for _, f := range []struct {
		bit  uint32
		name string
	}{
		{capProtocol41, "PROTOCOL_41"},
		{capDeprecateEOF, "DEPRECATE_EOF"},
		{capPluginAuth, "PLUGIN_AUTH"},
		{capSecureConn, "SECURE_CONNECTION"},
		{capTransactions, "TRANSACTIONS"},
		{capMultiStatements, "MULTI_STATEMENTS"},
		{capMultiResults, "MULTI_RESULTS"},
		{capConnectAttrs, "CONNECT_ATTRS"},
		{capQueryAttributes, "QUERY_ATTRIBUTES"},
		{capSSL, "SSL"},
		{capCompress, "COMPRESS"},
		{capZstdCompression, "ZSTD_COMPRESSION"},
	} {
		if caps&f.bit != 0 {
			notable = append(notable, f.name)
		}
	}
	ev.Summary = fmt.Sprintf("client login as %q (max packet %d, charset %d)", user, maxPacket, charset)
	ev.Info = strings.Join(notable, " | ")
	if r.err != nil {
		ev.DecodeErr = r.err.Error()
	}
}

func (d *connDecoder) deprecateEOF() bool { return d.caps&capDeprecateEOF != 0 }

func (d *connDecoder) decodeCommand(p *Packet, ev *Event) {
	if len(p.Payload) == 0 {
		ev.Kind, ev.Summary = "Empty", "empty client packet"
		return
	}
	// A command always starts a new request, identified by sequence id 0. Any
	// other sequence is a continuation of a multi-packet payload.
	if p.Seq != 0 {
		ev.Kind = "Continuation"
		ev.Summary = fmt.Sprintf("continuation of the previous request (%d bytes)", len(p.Payload))
		return
	}

	cmd := p.Payload[0]
	d.lastCommand = cmd
	name, known := commandNames[cmd]
	if !known {
		name = fmt.Sprintf("COM_UNKNOWN(0x%02x)", cmd)
	}
	ev.Kind = name

	r := newReader(p.Payload)
	r.byte()

	switch cmd {
	case 0x03: // COM_QUERY
		q := r.restOfPacket()
		d.lastQuery = q
		ev.Query = q
		ev.Summary = oneLine(q)
		d.state = respFirst
	case 0x16: // COM_STMT_PREPARE
		q := r.restOfPacket()
		d.pendingPrep = q
		ev.Query = q
		ev.Summary = "prepare: " + oneLine(q)
		d.state = respFirst
	case 0x17: // COM_STMT_EXECUTE
		id := r.uint32()
		q := d.stmts[id]
		ev.Query = q
		if q != "" {
			ev.Summary = fmt.Sprintf("execute stmt #%d: %s", id, oneLine(q))
		} else {
			ev.Summary = fmt.Sprintf("execute stmt #%d", id)
		}
		d.state = respFirst
	case 0x19: // COM_STMT_CLOSE — no response
		id := r.uint32()
		ev.Summary = fmt.Sprintf("close stmt #%d", id)
		delete(d.stmts, id)
		d.state = respNone
	case 0x1a: // COM_STMT_RESET
		ev.Summary = fmt.Sprintf("reset stmt #%d", r.uint32())
		d.state = respFirst
	case 0x02: // COM_INIT_DB
		db := r.restOfPacket()
		ev.Summary = "use database " + db
		d.state = respFirst
	case 0x01: // COM_QUIT — no response
		ev.Summary = "client closing the connection"
		d.state = respNone
	case 0x0e: // COM_PING
		ev.Summary = "ping"
		d.state = respFirst
	case 0x1f: // COM_RESET_CONNECTION
		ev.Summary = "reset session state"
		d.state = respFirst
	default:
		ev.Summary = name
		d.state = respFirst
	}
}

func (d *connDecoder) decodeResponse(p *Packet, ev *Event) {
	if len(p.Payload) == 0 {
		ev.Kind, ev.Summary = "Empty", "empty server packet"
		return
	}
	first := p.Payload[0]
	r := newReader(p.Payload)

	// An error packet is valid in any response state and always terminates it.
	if first == 0xFF {
		d.decodeErrPacket(r, ev)
		d.resetResult()
		return
	}

	switch d.state {
	case respFirst:
		d.decodeFirstResponse(p, first, r, ev)
	case respPrepareParams, respPrepareColumns, respColumnDefs:
		d.decodeDefinition(p, first, r, ev)
	case respRows:
		d.decodeRow(p, first, r, ev)
	default:
		ev.Kind = "Unsolicited"
		ev.Summary = fmt.Sprintf("server packet outside a known response (%d bytes)", len(p.Payload))
	}
}

func (d *connDecoder) decodeFirstResponse(p *Packet, first byte, r *reader, ev *Event) {
	switch {
	case first == 0x00 && d.lastCommand == 0x16:
		// COM_STMT_PREPARE_OK
		r.byte()
		id := r.uint32()
		numCols := int(r.uint16())
		numParams := int(r.uint16())
		r.skip(1)
		warnings := r.uint16()

		d.stmts[id] = d.pendingPrep
		ev.Kind = "PrepareOK"
		ev.Warnings = warnings
		ev.Query = d.pendingPrep
		ev.Summary = fmt.Sprintf("prepared as stmt #%d (%d params, %d columns)", id, numParams, numCols)
		d.pendingPrep = ""

		d.paramsWanted, d.paramsSeen = numParams, 0
		d.columnsWanted, d.columnsSeen = numCols, 0
		d.columns = nil
		switch {
		case numParams > 0:
			d.state = respPrepareParams
		case numCols > 0:
			d.state = respPrepareColumns
		default:
			d.state = respNone
		}

	case first == 0x00:
		d.decodeOKPacket(r, ev, false)

	case first == 0xFE && len(p.Payload) < 9:
		// With DEPRECATE_EOF this header carries an OK packet.
		d.decodeOKPacket(r, ev, true)

	case first == 0xFB:
		ev.Kind = "LocalInfileRequest"
		ev.Summary = "server requests a local file (LOAD DATA LOCAL INFILE)"
		d.state = respNone

	default:
		n, _ := r.lenEncInt()
		d.columnsWanted, d.columnsSeen = int(n), 0
		d.columns = nil
		d.rowCount = 0
		ev.Kind = "ResultSetHeader"
		ev.Summary = fmt.Sprintf("result set with %d column(s) follows", n)
		if n == 0 {
			d.state = respNone
			break
		}
		d.state = respColumnDefs
		d.awaitColumnEOF = !d.deprecateEOF()
	}
}

// decodeDefinition consumes a column definition packet, in either the result
// set or prepared statement metadata phase.
func (d *connDecoder) decodeDefinition(p *Packet, first byte, r *reader, ev *Event) {
	// An EOF terminator can appear between the parameter and column blocks of a
	// prepare response, and after the column block when DEPRECATE_EOF is off.
	if first == 0xFE && len(p.Payload) < 9 {
		ev.Kind = "EOF"
		ev.Summary = "end of metadata block"
		d.advanceAfterDefinitionEOF()
		return
	}

	def := decodeColumnDef(r)
	ev.Kind = "ColumnDefinition"
	if def.Table != "" {
		ev.Summary = fmt.Sprintf("column %s.%s (%s)", def.Table, def.Name, typeName(def.Type))
	} else {
		ev.Summary = fmt.Sprintf("column %s (%s)", def.Name, typeName(def.Type))
	}
	if def.unsigned() {
		ev.Summary += " unsigned"
	}
	if r.err != nil {
		ev.DecodeErr = r.err.Error()
	}

	switch d.state {
	case respPrepareParams:
		d.paramsSeen++
		if d.paramsSeen >= d.paramsWanted && d.deprecateEOF() {
			d.advanceAfterDefinitionEOF()
		}
	case respPrepareColumns:
		d.columnsSeen++
		if d.columnsSeen >= d.columnsWanted && d.deprecateEOF() {
			d.state = respNone
		}
	case respColumnDefs:
		d.columnsSeen++
		d.columns = append(d.columns, def)
		if d.columnsSeen >= d.columnsWanted {
			if d.awaitColumnEOF {
				// Wait for the explicit EOF before rows begin.
				return
			}
			d.state = respRows
		}
	}
}

// advanceAfterDefinitionEOF moves past a completed metadata block.
func (d *connDecoder) advanceAfterDefinitionEOF() {
	switch d.state {
	case respPrepareParams:
		if d.columnsWanted > 0 {
			d.state = respPrepareColumns
		} else {
			d.state = respNone
		}
	case respPrepareColumns:
		d.state = respNone
	case respColumnDefs:
		d.awaitColumnEOF = false
		d.state = respRows
	default:
		d.state = respNone
	}
}

func (d *connDecoder) decodeRow(p *Packet, first byte, r *reader, ev *Event) {
	if first == 0xFE && len(p.Payload) < 9 {
		total := d.rowCount
		if d.deprecateEOF() {
			d.decodeOKPacket(r, ev, true)
		} else {
			ev.Kind = "EOF"
			r.byte()
			ev.Warnings = r.uint16()
			ev.StatusFlags = statusFlagNames(r.uint16())
			d.state = respNone
		}
		ev.Summary = fmt.Sprintf("end of result set (%d row(s))", total)
		ev.Columns = d.columnNames()
		return
	}

	d.rowCount++
	ev.Kind = "Row"

	var vals []string
	if d.lastCommand == 0x17 {
		// Binary protocol: typed values behind a NULL bitmap.
		decoded, err := decodeBinaryRow(r, d.columns)
		vals = decoded
		ev.Protocol = "binary"
		if err != nil {
			ev.DecodeErr = err.Error()
		}
	} else {
		// Text protocol: every value is a length-encoded string.
		for r.remaining() > 0 && r.err == nil {
			s, isNull := r.lenEncString()
			if isNull {
				vals = append(vals, "NULL")
			} else {
				vals = append(vals, s)
			}
		}
		ev.Protocol = "text"
		if r.err != nil {
			ev.DecodeErr = r.err.Error()
		}
	}

	ev.Values = vals
	ev.Columns = d.columnNames()
	ev.Summary = fmt.Sprintf("row %d: %s", d.rowCount, oneLine(strings.Join(vals, " | ")))
}

// columnNames returns the current result set's column names.
func (d *connDecoder) columnNames() []string {
	if len(d.columns) == 0 {
		return nil
	}
	out := make([]string, len(d.columns))
	for i, c := range d.columns {
		out[i] = c.Name
	}
	return out
}

func (d *connDecoder) decodeOKPacket(r *reader, ev *Event, eofHeader bool) {
	r.byte() // 0x00 or 0xFE
	affected, _ := r.lenEncInt()
	lastInsert, _ := r.lenEncInt()
	status := r.uint16()
	warnings := r.uint16()
	info := strings.TrimSpace(r.restOfPacket())

	ev.Kind = "OK"
	ev.AffectedRows = affected
	ev.LastInsertID = lastInsert
	ev.Warnings = warnings
	ev.StatusFlags = statusFlagNames(status)
	if info != "" {
		ev.Info = info
	}
	parts := []string{fmt.Sprintf("%d row(s) affected", affected)}
	if lastInsert != 0 {
		parts = append(parts, fmt.Sprintf("last insert id %d", lastInsert))
	}
	if warnings != 0 {
		parts = append(parts, fmt.Sprintf("%d warning(s)", warnings))
	}
	if status&statusInTrans != 0 {
		parts = append(parts, "in transaction")
	}
	ev.Summary = strings.Join(parts, ", ")
	if eofHeader {
		ev.Kind = "OK"
	}

	if status&statusMoreResults != 0 {
		d.state = respFirst
	} else {
		d.resetResult()
	}
}

func (d *connDecoder) decodeErrPacket(r *reader, ev *Event) {
	r.byte() // 0xFF
	code := r.uint16()
	var sqlState string
	// Protocol 41 prefixes the SQL state with '#'.
	if r.remaining() >= 6 && r.b[r.pos] == '#' {
		r.byte()
		sqlState = string(r.fixed(5))
	}
	msg := r.restOfPacket()

	ev.Kind = "ERR"
	ev.ErrCode = code
	ev.SQLState = sqlState
	ev.ErrMessage = msg
	ev.Summary = fmt.Sprintf("error %d (%s): %s", code, sqlState, oneLine(msg))
}

func (d *connDecoder) resetResult() {
	d.state = respNone
	d.columnsWanted, d.columnsSeen = 0, 0
	d.paramsWanted, d.paramsSeen = 0, 0
	d.awaitColumnEOF = false
}

// MySQL column types, as they appear in ColumnDefinition41 and in binary
// result rows.
const (
	typeDecimal    = 0x00
	typeTiny       = 0x01
	typeShort      = 0x02
	typeLong       = 0x03
	typeFloat      = 0x04
	typeDouble     = 0x05
	typeNull       = 0x06
	typeTimestamp  = 0x07
	typeLongLong   = 0x08
	typeInt24      = 0x09
	typeDate       = 0x0a
	typeTime       = 0x0b
	typeDateTime   = 0x0c
	typeYear       = 0x0d
	typeVarchar    = 0x0f
	typeBit        = 0x10
	typeJSON       = 0xf5
	typeNewDecimal = 0xf6
	typeEnum       = 0xf7
	typeSet        = 0xf8
	typeTinyBlob   = 0xf9
	typeMediumBlob = 0xfa
	typeLongBlob   = 0xfb
	typeBlob       = 0xfc
	typeVarString  = 0xfd
	typeString     = 0xfe
	typeGeometry   = 0xff
)

const flagUnsigned = 0x0020

var typeNames = map[byte]string{
	typeDecimal: "DECIMAL", typeTiny: "TINYINT", typeShort: "SMALLINT",
	typeLong: "INT", typeFloat: "FLOAT", typeDouble: "DOUBLE", typeNull: "NULL",
	typeTimestamp: "TIMESTAMP", typeLongLong: "BIGINT", typeInt24: "MEDIUMINT",
	typeDate: "DATE", typeTime: "TIME", typeDateTime: "DATETIME", typeYear: "YEAR",
	typeVarchar: "VARCHAR", typeBit: "BIT", typeJSON: "JSON",
	typeNewDecimal: "DECIMAL", typeEnum: "ENUM", typeSet: "SET",
	typeTinyBlob: "TINYBLOB", typeMediumBlob: "MEDIUMBLOB", typeLongBlob: "LONGBLOB",
	typeBlob: "BLOB", typeVarString: "VARCHAR", typeString: "CHAR",
	typeGeometry: "GEOMETRY",
}

func typeName(t byte) string {
	if n, ok := typeNames[t]; ok {
		return n
	}
	return fmt.Sprintf("type 0x%02x", t)
}

// columnDef is the subset of ColumnDefinition41 needed to decode binary rows.
type columnDef struct {
	Name  string
	Table string
	Type  byte
	Flags uint16
}

func (c columnDef) unsigned() bool { return c.Flags&flagUnsigned != 0 }

// decodeColumnDef parses a ColumnDefinition41 packet.
func decodeColumnDef(r *reader) columnDef {
	_, _ = r.lenEncString() // catalog
	_, _ = r.lenEncString() // schema
	table, _ := r.lenEncString()
	_, _ = r.lenEncString() // org_table
	name, _ := r.lenEncString()
	_, _ = r.lenEncString() // org_name

	var def columnDef
	def.Name = name
	def.Table = table

	// A length-encoded 0x0c introduces the fixed-length block. Older or
	// truncated packets may stop here; the zero value is a safe fallback.
	if r.remaining() < 1 {
		return def
	}
	_, _ = r.lenEncInt() // length of the fixed-length fields (0x0c)
	r.skip(2)            // character set
	r.skip(4)            // column length
	def.Type = r.byte()
	def.Flags = r.uint16()
	r.skip(1) // decimals
	return def
}

// decodeBinaryRow decodes a binary protocol result row, which is what
// COM_STMT_EXECUTE returns.
//
// Layout: a 0x00 header, then a NULL bitmap of (columns + 7 + 2) / 8 bytes
// (offset by two reserved bits), then one value per non-NULL column encoded
// according to that column's type.
func decodeBinaryRow(r *reader, cols []columnDef) ([]string, error) {
	r.byte() // 0x00 packet header
	bitmapLen := (len(cols) + 7 + 2) / 8
	bitmap := r.fixed(bitmapLen)
	if bitmap == nil {
		return nil, r.err
	}

	vals := make([]string, 0, len(cols))
	for i, col := range cols {
		// The bitmap's first two bits are reserved, so column i lives at i+2.
		if bitmap[(i+2)/8]&(1<<uint((i+2)%8)) != 0 {
			vals = append(vals, "NULL")
			continue
		}
		vals = append(vals, decodeBinaryValue(r, col))
		if r.err != nil {
			return vals, r.err
		}
	}
	return vals, nil
}

func decodeBinaryValue(r *reader, col columnDef) string {
	switch col.Type {
	case typeTiny:
		b := r.byte()
		if col.unsigned() {
			return strconv.FormatUint(uint64(b), 10)
		}
		return strconv.FormatInt(int64(int8(b)), 10)

	case typeShort, typeYear:
		v := r.uint16()
		if col.unsigned() {
			return strconv.FormatUint(uint64(v), 10)
		}
		return strconv.FormatInt(int64(int16(v)), 10)

	case typeLong, typeInt24:
		v := r.uint32()
		if col.unsigned() {
			return strconv.FormatUint(uint64(v), 10)
		}
		return strconv.FormatInt(int64(int32(v)), 10)

	case typeLongLong:
		b := r.fixed(8)
		if b == nil {
			return ""
		}
		v := binary.LittleEndian.Uint64(b)
		if col.unsigned() {
			return strconv.FormatUint(v, 10)
		}
		return strconv.FormatInt(int64(v), 10)

	case typeFloat:
		b := r.fixed(4)
		if b == nil {
			return ""
		}
		return strconv.FormatFloat(float64(math.Float32frombits(binary.LittleEndian.Uint32(b))), 'g', -1, 32)

	case typeDouble:
		b := r.fixed(8)
		if b == nil {
			return ""
		}
		return strconv.FormatFloat(math.Float64frombits(binary.LittleEndian.Uint64(b)), 'g', -1, 64)

	case typeNull:
		return "NULL"

	case typeDate, typeDateTime, typeTimestamp:
		return decodeBinaryDate(r)

	case typeTime:
		return decodeBinaryTime(r)

	default:
		// Everything else -- strings, blobs, decimals, JSON, bit, enum, set,
		// geometry -- is a length-encoded string on the wire.
		s, isNull := r.lenEncString()
		if isNull {
			return "NULL"
		}
		return s
	}
}

func decodeBinaryDate(r *reader) string {
	n := r.byte()
	switch n {
	case 0:
		return "0000-00-00"
	case 4:
		b := r.fixed(4)
		if b == nil {
			return ""
		}
		return fmt.Sprintf("%04d-%02d-%02d", binary.LittleEndian.Uint16(b), b[2], b[3])
	case 7:
		b := r.fixed(7)
		if b == nil {
			return ""
		}
		return fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d",
			binary.LittleEndian.Uint16(b), b[2], b[3], b[4], b[5], b[6])
	case 11:
		b := r.fixed(11)
		if b == nil {
			return ""
		}
		return fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d.%06d",
			binary.LittleEndian.Uint16(b), b[2], b[3], b[4], b[5], b[6],
			binary.LittleEndian.Uint32(b[7:]))
	default:
		r.skip(int(n))
		return fmt.Sprintf("<unexpected %d-byte date>", n)
	}
}

func decodeBinaryTime(r *reader) string {
	n := r.byte()
	switch n {
	case 0:
		return "00:00:00"
	case 8, 12:
		b := r.fixed(int(n))
		if b == nil {
			return ""
		}
		sign := ""
		if b[0] == 1 {
			sign = "-"
		}
		days := binary.LittleEndian.Uint32(b[1:5])
		hours := uint32(b[5]) + days*24
		out := fmt.Sprintf("%s%02d:%02d:%02d", sign, hours, b[6], b[7])
		if n == 12 {
			out += fmt.Sprintf(".%06d", binary.LittleEndian.Uint32(b[8:12]))
		}
		return out
	default:
		r.skip(int(n))
		return fmt.Sprintf("<unexpected %d-byte time>", n)
	}
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 160 {
		s = s[:159] + "…"
	}
	return s
}
