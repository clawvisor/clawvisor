// Command imessage-helper is a small, stable binary that reads the macOS
// Messages database (~/Library/Messages/chat.db) and the Contacts database.
//
// It is separated from the main clawvisor binary so that Full Disk Access
// (FDA) persists across clawvisor updates — macOS ties FDA to the specific
// binary, and this helper rarely changes.
//
// Protocol: reads a single JSON request from stdin, writes a JSON response
// to stdout, then exits.
//
//	Request:  {"action":"search_messages","params":{...}}
//	Response: {"summary":"...","data":[...]}
//	Error:    {"error":"..."}
package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

// ProtocolVersion is bumped only when the helper's stdin/stdout contract or
// behavior changes in a way that requires a new binary. The adapter in the
// main clawvisor binary checks this to decide whether to download an update.
// Changing this version means users will need to re-grant Full Disk Access.
const ProtocolVersion = "1"

// ── Protocol types ───────────────────────────────────────────────────────────

type helperRequest struct {
	Action string         `json:"action"`
	Params map[string]any `json:"params"`
}

type helperResponse struct {
	Summary string `json:"summary,omitempty"`
	Data    any    `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
}

// ── Result types (match the adapter's JSON contract) ─────────────────────────

type messageResult struct {
	ID             string `json:"id"`
	From           string `json:"from"`
	FromIdentifier string `json:"from_identifier"`
	Text           string `json:"text"`
	Timestamp      string `json:"timestamp"`
	IsFromMe       bool   `json:"is_from_me"`
	ThreadID       string `json:"thread_id"`
}

type threadItem struct {
	ThreadID           string `json:"thread_id"`
	DisplayName        string `json:"display_name"`
	LastMessageSnippet string `json:"last_message_snippet"`
	LastMessageAt      string `json:"last_message_at"`
	ParticipantCount   int    `json:"participant_count"`
}

// ── Main ─────────────────────────────────────────────────────────────────────

func main() {
	if runtime.GOOS != "darwin" {
		writeError("imessage-helper: only available on macOS")
		return
	}

	var req helperRequest
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
		writeError(fmt.Sprintf("invalid request: %v", err))
		return
	}

	home, _ := os.UserHomeDir()
	dbPath := filepath.Join(home, "Library", "Messages", "chat.db")

	resp, err := dispatch(dbPath, req)
	if err != nil {
		writeError(err.Error())
		return
	}
	json.NewEncoder(os.Stdout).Encode(resp)
}

func writeError(msg string) {
	json.NewEncoder(os.Stdout).Encode(helperResponse{Error: msg})
}

func dispatch(dbPath string, req helperRequest) (*helperResponse, error) {
	switch req.Action {
	case "version":
		return &helperResponse{Data: map[string]string{"protocol_version": ProtocolVersion}}, nil
	case "check_permissions":
		return checkPermissions(dbPath)
	case "search_messages":
		return searchMessages(dbPath, req.Params)
	case "list_threads":
		return listThreads(dbPath, req.Params)
	case "get_thread":
		return getThread(dbPath, req.Params)
	case "send_message":
		return sendMessage(dbPath, req.Params)
	default:
		return nil, fmt.Errorf("unsupported action %q", req.Action)
	}
}

// ── check_permissions ────────────────────────────────────────────────────────

func checkPermissions(dbPath string) (*helperResponse, error) {
	db, cleanup, err := openDB(dbPath)
	if err != nil {
		return nil, fmt.Errorf("cannot open chat.db: %w — grant Full Disk Access in System Settings → Privacy & Security → Full Disk Access", err)
	}
	defer cleanup()
	defer db.Close()
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("cannot read chat.db: %w — grant Full Disk Access in System Settings → Privacy & Security → Full Disk Access", err)
	}
	return &helperResponse{Summary: "ok"}, nil
}

// ── search_messages ──────────────────────────────────────────────────────────

func searchMessages(dbPath string, params map[string]any) (*helperResponse, error) {
	query, _ := params["query"].(string)
	if query == "" {
		return nil, fmt.Errorf("search_messages: query is required")
	}
	contact, _ := params["contact"].(string)
	daysBack := 90
	if v, ok := paramInt(params, "days_back"); ok && v > 0 {
		daysBack = v
	}
	maxResults := 20
	if v, ok := paramInt(params, "max_results"); ok && v > 0 && v <= 50 {
		maxResults = v
	}

	db, cleanup, err := openDB(dbPath)
	if err != nil {
		return nil, fmt.Errorf("cannot open chat.db: %w", err)
	}
	defer cleanup()
	defer db.Close()

	since := time.Now().Add(-time.Duration(daysBack) * 24 * time.Hour)
	coredataEpoch := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	sinceApple := since.Sub(coredataEpoch).Nanoseconds()

	var sqlQuery string
	var args []any

	if contact != "" {
		identifiers, _ := resolveContactIdentifiers(db, contact)
		if len(identifiers) == 0 {
			return &helperResponse{
				Summary: fmt.Sprintf("No messages from %q matching %q", contact, query),
				Data:    []messageResult{},
			}, nil
		}
		placeholders := make([]string, len(identifiers))
		for i, id := range identifiers {
			placeholders[i] = "?"
			args = append(args, id)
		}
		likePattern := "%" + query + "%"
		args = append(args, likePattern, likePattern, sinceApple, maxResults)
		sqlQuery = fmt.Sprintf(`
			SELECT m.ROWID, m.text, m.is_from_me, m.date, h.id, c.chat_identifier, m.attributedBody
			FROM message m
			JOIN chat_message_join cmj ON cmj.message_id = m.ROWID
			JOIN chat c ON c.ROWID = cmj.chat_id
			LEFT JOIN handle h ON h.ROWID = m.handle_id
			WHERE h.id IN (%s)
			  AND (m.text LIKE ? OR m.attributedBody LIKE ?)
			  AND m.date > ?
			  AND m.is_from_me = 0
			ORDER BY m.date DESC
			LIMIT ?`, strings.Join(placeholders, ","))
	} else {
		likePattern := "%" + query + "%"
		args = []any{likePattern, likePattern, sinceApple, maxResults}
		sqlQuery = `
			SELECT m.ROWID, m.text, m.is_from_me, m.date, COALESCE(h.id, ''), c.chat_identifier, m.attributedBody
			FROM message m
			JOIN chat_message_join cmj ON cmj.message_id = m.ROWID
			JOIN chat c ON c.ROWID = cmj.chat_id
			LEFT JOIN handle h ON h.ROWID = m.handle_id
			WHERE (m.text LIKE ? OR m.attributedBody LIKE ?)
			  AND m.date > ?
			ORDER BY m.date DESC
			LIMIT ?`
	}

	rows, err := db.Query(sqlQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("search_messages query: %w", err)
	}
	defer rows.Close()

	msgs := scanMessages(rows)
	nameMap := buildNameMap(db, msgs)
	results := formatMessages(msgs, nameMap)

	return &helperResponse{
		Summary: fmt.Sprintf("%d message(s) matching %q", len(results), query),
		Data:    results,
	}, nil
}

// ── list_threads ─────────────────────────────────────────────────────────────

func listThreads(dbPath string, params map[string]any) (*helperResponse, error) {
	maxResults := 20
	if v, ok := paramInt(params, "max_results"); ok && v > 0 && v <= 50 {
		maxResults = v
	}

	db, cleanup, err := openDB(dbPath)
	if err != nil {
		return nil, fmt.Errorf("cannot open chat.db: %w", err)
	}
	defer cleanup()
	defer db.Close()

	rows, err := db.Query(`
		SELECT c.chat_identifier, c.display_name,
		       (SELECT m2.text FROM message m2
		        JOIN chat_message_join cmj2 ON cmj2.message_id = m2.ROWID
		        WHERE cmj2.chat_id = c.ROWID ORDER BY m2.date DESC LIMIT 1) as last_text,
		       (SELECT m2.attributedBody FROM message m2
		        JOIN chat_message_join cmj2 ON cmj2.message_id = m2.ROWID
		        WHERE cmj2.chat_id = c.ROWID ORDER BY m2.date DESC LIMIT 1) as last_attr_body,
		       MAX(m.date) as last_date,
		       COUNT(DISTINCT ch.handle_id) as participant_count
		FROM chat c
		JOIN chat_message_join cmj ON cmj.chat_id = c.ROWID
		JOIN message m ON m.ROWID = cmj.message_id
		LEFT JOIN chat_handle_join ch ON ch.chat_id = c.ROWID
		WHERE m.date > 0
		GROUP BY c.ROWID
		ORDER BY last_date DESC
		LIMIT ?`, maxResults)
	if err != nil {
		return nil, fmt.Errorf("list_threads query: %w", err)
	}
	defer rows.Close()

	coredataEpoch := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	items := make([]threadItem, 0)
	for rows.Next() {
		var threadID, displayName string
		var lastText sql.NullString
		var lastAttrBody []byte
		var lastDateNS int64
		var participantCount int
		if err := rows.Scan(&threadID, &displayName, &lastText, &lastAttrBody, &lastDateNS, &participantCount); err != nil {
			continue
		}
		lastAt := coredataEpoch.Add(time.Duration(lastDateNS))
		name := displayName
		if name == "" {
			name = threadID
		}
		snippet := ""
		if lastText.Valid {
			snippet = strings.TrimSpace(lastText.String)
		}
		if snippet == "" {
			snippet = extractTextFromAttributedBody(lastAttrBody)
		}
		if snippet != "" {
			snippet = truncateRunes(snippet, 300)
		}
		items = append(items, threadItem{
			ThreadID:           threadID,
			DisplayName:        truncateRunes(name, 500),
			LastMessageSnippet: snippet,
			LastMessageAt:      lastAt.UTC().Format(time.RFC3339),
			ParticipantCount:   participantCount,
		})
	}

	// Resolve group chat display names from Address Book participants.
	var unresolvedIDs []string
	for i := range items {
		if items[i].DisplayName == items[i].ThreadID {
			unresolvedIDs = append(unresolvedIDs, items[i].ThreadID)
		}
	}
	if len(unresolvedIDs) > 0 {
		resolved := resolveThreadDisplayNames(db, unresolvedIDs)
		for i := range items {
			if name, ok := resolved[items[i].ThreadID]; ok {
				items[i].DisplayName = truncateRunes(name, 500)
			}
		}
	}

	return &helperResponse{
		Summary: fmt.Sprintf("%d recent conversation(s)", len(items)),
		Data:    items,
	}, nil
}

// ── get_thread ───────────────────────────────────────────────────────────────

func getThread(dbPath string, params map[string]any) (*helperResponse, error) {
	contact, _ := params["contact"].(string)
	threadID, _ := params["thread_id"].(string)
	if contact == "" && threadID == "" {
		return nil, fmt.Errorf("get_thread: contact or thread_id is required")
	}
	maxResults := 50
	if v, ok := paramInt(params, "max_results"); ok && v > 0 && v <= 200 {
		maxResults = v
	}
	daysBack := 30
	if v, ok := paramInt(params, "days_back"); ok && v > 0 {
		daysBack = v
	}

	db, cleanup, err := openDB(dbPath)
	if err != nil {
		return nil, fmt.Errorf("cannot open chat.db: %w", err)
	}
	defer cleanup()
	defer db.Close()

	coredataEpoch := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	since := time.Now().Add(-time.Duration(daysBack) * 24 * time.Hour)
	sinceApple := since.Sub(coredataEpoch).Nanoseconds()

	var sqlQuery string
	var args []any

	if contact != "" {
		identifiers, _ := resolveContactIdentifiers(db, contact)
		if len(identifiers) == 0 {
			identifiers = []string{contact}
		}
		placeholders := make([]string, len(identifiers))
		for i, id := range identifiers {
			placeholders[i] = "?"
			args = append(args, id)
		}
		args = append(args, sinceApple, maxResults)
		sqlQuery = fmt.Sprintf(`
			SELECT m.ROWID, m.text, m.is_from_me, m.date, COALESCE(h.id, ''), c.chat_identifier, m.attributedBody
			FROM message m
			JOIN chat_message_join cmj ON cmj.message_id = m.ROWID
			JOIN chat c ON c.ROWID = cmj.chat_id
			LEFT JOIN handle h ON h.ROWID = m.handle_id
			JOIN chat_handle_join chj ON chj.chat_id = c.ROWID
			JOIN handle ch ON ch.ROWID = chj.handle_id AND ch.id IN (%s)
			WHERE m.date > ?
			ORDER BY m.date DESC
			LIMIT ?`, strings.Join(placeholders, ","))
	} else {
		args = []any{threadID, sinceApple, maxResults}
		sqlQuery = `
			SELECT m.ROWID, m.text, m.is_from_me, m.date, COALESCE(h.id, ''), c.chat_identifier, m.attributedBody
			FROM message m
			JOIN chat_message_join cmj ON cmj.message_id = m.ROWID
			JOIN chat c ON c.ROWID = cmj.chat_id
			LEFT JOIN handle h ON h.ROWID = m.handle_id
			WHERE c.chat_identifier = ?
			  AND m.date > ?
			ORDER BY m.date DESC
			LIMIT ?`
	}

	rows, err := db.Query(sqlQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("get_thread query: %w", err)
	}
	defer rows.Close()

	msgs := scanMessages(rows)
	nameMap := buildNameMap(db, msgs)
	messages := formatMessages(msgs, nameMap)

	displayName := contact
	if displayName == "" {
		displayName = threadID
		if resolved := resolveThreadDisplayNames(db, []string{threadID}); resolved[threadID] != "" {
			displayName = resolved[threadID]
		}
	}
	result := map[string]any{
		"thread_id": threadID,
		"contact":   displayName,
		"messages":  messages,
	}
	return &helperResponse{
		Summary: fmt.Sprintf("Last %d days of messages with %s (%d messages)", daysBack, displayName, len(messages)),
		Data:    result,
	}, nil
}

// ── send_message ─────────────────────────────────────────────────────────────

func sendMessage(dbPath string, params map[string]any) (*helperResponse, error) {
	to, _ := params["to"].(string)
	text, _ := params["text"].(string)
	if text == "" {
		text, _ = params["body"].(string)
	}
	if to == "" {
		return nil, fmt.Errorf("send_message: to is required")
	}
	if text == "" {
		return nil, fmt.Errorf("send_message: text is required")
	}
	if len(text) > 2000 {
		return nil, fmt.Errorf("send_message: text exceeds 2000 characters")
	}

	identifier := to
	db, cleanup, dbErr := openDB(dbPath)
	if dbErr == nil {
		defer cleanup()
		defer db.Close()
		identifiers, _ := resolveContactIdentifiers(db, to)
		if len(identifiers) > 0 {
			identifier = identifiers[0]
		}
	}

	script := `on run argv
	tell application "Messages"
		set targetService to 1st service whose service type = iMessage
		set targetBuddy to buddy (item 1 of argv) of targetService
		send (item 2 of argv) to targetBuddy
	end tell
end run`

	cmd := exec.Command("osascript", "-e", script, "--", identifier, text)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("send_message: AppleScript failed: %w — %s", err, truncate(string(out), 200))
	}
	return &helperResponse{
		Summary: fmt.Sprintf("iMessage sent to %s", to),
		Data:    map[string]string{"to": to, "to_identifier": identifier},
	}, nil
}

// ── Database helpers ─────────────────────────────────────────────────────────

// openDB snapshots chat.db (+ WAL file) into a temp directory and opens it
// read-only. This sidesteps the SQLITE_CANTOPEN / "out of memory" error that
// occurs when modernc.org/sqlite tries to mmap the .db-shm shared-memory file
// for a WAL-mode database owned by Messages.app.
func openDB(dbPath string) (*sql.DB, func(), error) {
	tmpDir, err := os.MkdirTemp("", "cw-imsg-*")
	if err != nil {
		return nil, nil, fmt.Errorf("temp dir: %w", err)
	}
	cleanup := func() {
		_ = os.RemoveAll(tmpDir)
	}

	tmpDB := filepath.Join(tmpDir, "chat.db")
	if err := copyFile(dbPath, tmpDB); err != nil {
		cleanup()
		if os.IsPermission(err) {
			return nil, nil, fmt.Errorf("cannot read chat.db: %w — grant Full Disk Access in System Settings → Privacy & Security → Full Disk Access", err)
		}
		return nil, nil, fmt.Errorf("copy chat.db: %w", err)
	}
	if _, serr := os.Stat(dbPath + "-wal"); serr == nil {
		_ = copyFile(dbPath+"-wal", tmpDB+"-wal")
	}

	db, err := sql.Open("sqlite", "file:"+tmpDB+"?mode=ro")
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return db, cleanup, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// ── Message scanning & formatting ────────────────────────────────────────────

type rawMessage struct {
	rowID          int64
	text           sql.NullString
	attributedBody []byte
	isFromMe       bool
	dateNS         int64
	handleID       string
	chatID         string
}

func scanMessages(rows *sql.Rows) []rawMessage {
	var msgs []rawMessage
	for rows.Next() {
		var m rawMessage
		var isFromMeInt int
		var attrBody []byte
		if err := rows.Scan(&m.rowID, &m.text, &isFromMeInt, &m.dateNS, &m.handleID, &m.chatID, &attrBody); err != nil {
			continue
		}
		m.attributedBody = attrBody
		m.isFromMe = isFromMeInt != 0
		msgs = append(msgs, m)
	}
	return msgs
}

func formatMessages(msgs []rawMessage, nameMap map[string]string) []messageResult {
	coredataEpoch := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	results := make([]messageResult, 0, len(msgs))
	for _, m := range msgs {
		text := ""
		if m.text.Valid {
			text = strings.TrimSpace(m.text.String)
		}
		if extracted := extractTextFromAttributedBody(m.attributedBody); extracted != "" {
			if text == "" || isOnlyObjectReplacement(text) || len(extracted) > len(text) {
				text = extracted
			}
		}
		if text == "" || isOnlyObjectReplacement(text) {
			text = "[attachment]"
		}
		ts := coredataEpoch.Add(time.Duration(m.dateNS))
		from := m.handleID
		displayName := nameMap[m.handleID]
		if displayName == "" {
			displayName = m.handleID
		}
		if m.isFromMe {
			displayName = "me"
			from = "me"
		}
		results = append(results, messageResult{
			ID:             fmt.Sprintf("msg-%d", m.rowID),
			From:           displayName,
			FromIdentifier: from,
			Text:           truncateRunes(text, 200_000),
			Timestamp:      ts.UTC().Format(time.RFC3339),
			IsFromMe:       m.isFromMe,
			ThreadID:       m.chatID,
		})
	}
	return results
}

func buildNameMap(db *sql.DB, msgs []rawMessage) map[string]string {
	ids := make(map[string]bool)
	for _, m := range msgs {
		if !m.isFromMe && m.handleID != "" {
			ids[m.handleID] = true
		}
	}
	return lookupHandleNames(ids)
}

// ── Contact resolution ───────────────────────────────────────────────────────

func lookupHandleNames(handleIDs map[string]bool) map[string]string {
	nameMap := make(map[string]string)
	if len(handleIDs) == 0 {
		return nameMap
	}

	abPaths, _ := filepath.Glob(filepath.Join(os.Getenv("HOME"),
		"Library/Application Support/AddressBook/Sources/*/AddressBook-v22.abcddb"))
	if len(abPaths) == 0 {
		return nameMap
	}

	abDB, err := sql.Open("sqlite", "file:"+abPaths[0]+"?mode=ro&immutable=1")
	if err != nil {
		return nameMap
	}
	defer abDB.Close()

	for id := range handleIDs {
		var firstName, lastName sql.NullString
		digits := normalizePhone(id)
		digits = strings.TrimPrefix(digits, "+")
		if len(digits) > 10 {
			digits = digits[len(digits)-10:]
		}
		err := abDB.QueryRow(`
			SELECT p.ZFIRSTNAME, p.ZLASTNAME
			FROM ZABCDRECORD p
			JOIN ZABCDPHONENUMBER pn ON pn.ZOWNER = p.Z_PK
			WHERE REPLACE(REPLACE(REPLACE(REPLACE(REPLACE(
			        pn.ZFULLNUMBER, ' ', ''), '-', ''), '(', ''), ')', ''), '+', '')
			      LIKE ?
			LIMIT 1`, "%"+digits+"%").
			Scan(&firstName, &lastName)
		if err != nil {
			err = abDB.QueryRow(`
				SELECT p.ZFIRSTNAME, p.ZLASTNAME
				FROM ZABCDRECORD p
				JOIN ZABCDEMAILADDRESS ea ON ea.ZOWNER = p.Z_PK
				WHERE lower(ea.ZADDRESS) = lower(?)
				LIMIT 1`, id).Scan(&firstName, &lastName)
		}
		if err == nil {
			parts := []string{}
			if firstName.Valid {
				parts = append(parts, firstName.String)
			}
			if lastName.Valid {
				parts = append(parts, lastName.String)
			}
			if len(parts) > 0 {
				nameMap[id] = strings.Join(parts, " ")
			}
		}
	}
	return nameMap
}

func resolveThreadDisplayNames(db *sql.DB, threadIDs []string) map[string]string {
	if len(threadIDs) == 0 {
		return nil
	}

	placeholders := make([]string, len(threadIDs))
	args := make([]any, len(threadIDs))
	for i, id := range threadIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	rows, err := db.Query(fmt.Sprintf(`
		SELECT c.chat_identifier, h.id
		FROM handle h
		JOIN chat_handle_join chj ON chj.handle_id = h.ROWID
		JOIN chat c ON c.ROWID = chj.chat_id
		WHERE c.chat_identifier IN (%s)`, strings.Join(placeholders, ",")), args...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	threadHandles := make(map[string][]string)
	allHandles := make(map[string]bool)
	for rows.Next() {
		var chatID, handleID string
		if err := rows.Scan(&chatID, &handleID); err != nil {
			continue
		}
		threadHandles[chatID] = append(threadHandles[chatID], handleID)
		allHandles[handleID] = true
	}

	nameMap := lookupHandleNames(allHandles)

	result := make(map[string]string, len(threadIDs))
	for _, tid := range threadIDs {
		handles := threadHandles[tid]
		if len(handles) == 0 {
			continue
		}
		names := make([]string, 0, len(handles))
		for _, h := range handles {
			if n, ok := nameMap[h]; ok {
				names = append(names, n)
			} else {
				names = append(names, h)
			}
		}
		const maxNames = 4
		if len(names) > maxNames {
			result[tid] = strings.Join(names[:maxNames], ", ") + fmt.Sprintf(" + %d more", len(names)-maxNames)
		} else {
			result[tid] = strings.Join(names, ", ")
		}
	}
	return result
}

func resolveContactIdentifiers(db *sql.DB, contact string) ([]string, error) {
	rows, err := db.Query(`SELECT DISTINCT id FROM handle WHERE id LIKE ?`,
		"%"+contact+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}

	abPaths, _ := filepath.Glob(filepath.Join(os.Getenv("HOME"),
		"Library/Application Support/AddressBook/Sources/*/AddressBook-v22.abcddb"))
	if len(abPaths) > 0 {
		abDB, err := sql.Open("sqlite", "file:"+abPaths[0]+"?mode=ro&immutable=1")
		if err == nil {
			defer abDB.Close()
			abRows, err := abDB.Query(`
				SELECT pn.ZFULLNUMBER, ea.ZADDRESS
				FROM ZABCDRECORD p
				LEFT JOIN ZABCDPHONENUMBER pn ON pn.ZOWNER = p.Z_PK
				LEFT JOIN ZABCDEMAILADDRESS ea ON ea.ZOWNER = p.Z_PK
				WHERE (p.ZFIRSTNAME || ' ' || COALESCE(p.ZLASTNAME,'')) LIKE ?
				   OR p.ZFIRSTNAME LIKE ?
				   OR p.ZLASTNAME LIKE ?`,
				"%"+contact+"%", "%"+contact+"%", "%"+contact+"%")
			if err == nil {
				defer abRows.Close()
				for abRows.Next() {
					var phone, email sql.NullString
					if err := abRows.Scan(&phone, &email); err == nil {
						if phone.Valid && phone.String != "" {
							ids = append(ids, normalizePhone(phone.String))
						}
						if email.Valid && email.String != "" {
							ids = append(ids, strings.ToLower(email.String))
						}
					}
				}
			}
		}
	}
	return ids, nil
}

// ── Text extraction (typedstream / NSAttributedString) ───────────────────────

func extractTextFromAttributedBody(data []byte) string {
	if len(data) < 30 {
		return ""
	}

	for _, className := range []string{"NSString", "NSMutableString"} {
		idx := bytes.Index(data, []byte(className))
		if idx < 0 {
			continue
		}
		start := idx + len(className)

		if s := scanForStringMarker(data, start, 0x2B); s != "" {
			return s
		}
		if s := scanForLengthPrefixedUTF8(data, start); s != "" {
			return s
		}
	}

	if s := scanForStringMarker(data, 0, 0x2B); s != "" && containsWhitespace(s) {
		return s
	}
	if s := scanForLengthPrefixedUTF8(data, 0); s != "" && containsWhitespace(s) {
		return s
	}

	return ""
}

func scanForStringMarker(data []byte, start int, marker byte) string {
	var best string
	for i := start; i < len(data)-1; i++ {
		if data[i] != marker {
			continue
		}

		length, skip := readTypedStreamLength(data[i+1:])
		if length <= 0 || i+1+skip+length > len(data) {
			continue
		}

		candidate := data[i+1+skip : i+1+skip+length]
		if utf8.Valid(candidate) {
			s := stripObjectReplacement(string(candidate))
			if s != "" && len(s) > len(best) {
				best = s
			}
		}
	}
	return best
}

func scanForLengthPrefixedUTF8(data []byte, start int) string {
	var best string
	for i := start; i < len(data)-1; i++ {
		length, skip := readTypedStreamLength(data[i:])
		if length < 2 || i+skip+length > len(data) {
			continue
		}
		candidate := data[i+skip : i+skip+length]
		if !utf8.Valid(candidate) {
			continue
		}
		s := stripObjectReplacement(string(candidate))
		if len(s) > len(best) && looksLikeText(s) {
			best = s
		}
	}
	return best
}

func readTypedStreamLength(data []byte) (int, int) {
	if len(data) == 0 {
		return 0, 0
	}
	b := data[0]
	if b < 0x80 {
		return int(b), 1
	}
	var nBytes int
	switch b {
	case 0x81:
		nBytes = 2
	case 0x82:
		nBytes = 4
	default:
		return 0, 0
	}
	if 1+nBytes > len(data) {
		return 0, 0
	}
	length := 0
	for j := 0; j < nBytes; j++ {
		length |= int(data[1+j]) << (8 * j)
	}
	return length, 1 + nBytes
}

// ── String helpers ───────────────────────────────────────────────────────────

func normalizePhone(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' || r == '+' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func paramInt(params map[string]any, key string) (int, bool) {
	v, ok := params[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	}
	return 0, false
}

func isOnlyObjectReplacement(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r != '\uFFFC' {
			return false
		}
	}
	return true
}

func stripObjectReplacement(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r != '\uFFFC' {
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

func looksLikeText(s string) bool {
	if len(s) < 2 {
		return false
	}
	printable := 0
	total := 0
	for _, r := range s {
		total++
		if r >= ' ' || r == '\n' || r == '\r' || r == '\t' {
			printable++
		}
	}
	return total > 0 && float64(printable)/float64(total) >= 0.8
}

func containsWhitespace(s string) bool {
	return strings.ContainsAny(s, " \n\t")
}

func truncate(s string, max int) string {
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

func truncateRunes(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxRunes]) + " [truncated]"
}
