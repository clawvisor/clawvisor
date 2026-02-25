// Package imessage implements the Clawvisor adapter for Apple iMessage.
// This adapter reads directly from the macOS Messages SQLite database
// (~/Library/Messages/chat.db) and optionally resolves contact names
// from the macOS Contacts database.
//
// Prerequisites:
//   - macOS with Messages.app configured
//   - Full Disk Access permission for the Clawvisor process
//
// The send_message action always requires human approval, regardless of policy.
// Sending is performed via AppleScript; Messages.app must be running.
package imessage

import (
	"bytes"
	"context"
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

	"golang.org/x/oauth2"

	_ "modernc.org/sqlite" // registers "sqlite" driver

	"github.com/clawvisor/clawvisor/internal/adapters"
	"github.com/clawvisor/clawvisor/internal/adapters/format"
)

const serviceID = "apple.imessage"

// IMessageAdapter implements adapters.Adapter for Apple iMessage.
type IMessageAdapter struct {
	dbPath string
}

func New() *IMessageAdapter {
	home, _ := os.UserHomeDir()
	return &IMessageAdapter{
		dbPath: filepath.Join(home, "Library", "Messages", "chat.db"),
	}
}

// Available returns true if the adapter can operate on this host.
// Requires macOS with chat.db readable (Full Disk Access must be granted).
func (a *IMessageAdapter) Available() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	f, err := os.Open(a.dbPath)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

func (a *IMessageAdapter) ServiceID() string { return serviceID }

func (a *IMessageAdapter) SupportedActions() []string {
	return []string{"search_messages", "list_threads", "get_thread", "send_message"}
}

// OAuthConfig returns nil — iMessage uses local file access, no OAuth.
func (a *IMessageAdapter) OAuthConfig() *oauth2.Config { return nil }

// RequiredScopes returns nil — iMessage uses local file access, not OAuth scopes.
func (a *IMessageAdapter) RequiredScopes() []string { return nil }

// CredentialFromToken is unused for local services.
func (a *IMessageAdapter) CredentialFromToken(_ *oauth2.Token) ([]byte, error) {
	return nil, fmt.Errorf("imessage: no token exchange — local service")
}

// ValidateCredential accepts any non-nil byte slice (no stored credential needed).
func (a *IMessageAdapter) ValidateCredential(_ []byte) error { return nil }

func (a *IMessageAdapter) Execute(ctx context.Context, req adapters.Request) (*adapters.Result, error) {
	switch req.Action {
	case "search_messages":
		return a.searchMessages(ctx, req.Params)
	case "list_threads":
		return a.listThreads(ctx, req.Params)
	case "get_thread":
		return a.getThread(ctx, req.Params)
	case "send_message":
		return a.sendMessage(ctx, req.Params)
	default:
		return nil, fmt.Errorf("imessage: unsupported action %q", req.Action)
	}
}

// CheckPermissions tries to open chat.db read-only.
// Returns an error with human-readable guidance if access is denied.
func (a *IMessageAdapter) CheckPermissions() error {
	db, cleanup, err := a.openDB()
	if err != nil {
		return fmt.Errorf("cannot open chat.db: %w — grant Full Disk Access in System Settings → Privacy & Security → Full Disk Access", err)
	}
	defer cleanup()
	defer db.Close()
	if err := db.Ping(); err != nil {
		return fmt.Errorf("cannot read chat.db: %w — grant Full Disk Access in System Settings → Privacy & Security → Full Disk Access", err)
	}
	return nil
}

// openDB snapshots chat.db (+ WAL file) into a temp directory and opens it
// read-only. This sidesteps the SQLITE_CANTOPEN / "out of memory" error that
// occurs when modernc.org/sqlite tries to mmap the .db-shm shared-memory file
// for a WAL-mode database owned by Messages.app.
//
// The caller must invoke the returned cleanup function when done.
func (a *IMessageAdapter) openDB() (*sql.DB, func(), error) {
	tmpDir, err := os.MkdirTemp("", "cw-imsg-*")
	if err != nil {
		return nil, nil, fmt.Errorf("temp dir: %w", err)
	}
	cleanup := func() {
		_ = os.RemoveAll(tmpDir)
	}

	tmpDB := filepath.Join(tmpDir, "chat.db")
	if err := copyFile(a.dbPath, tmpDB); err != nil {
		cleanup()
		if os.IsPermission(err) {
			return nil, nil, fmt.Errorf("cannot read chat.db: %w — grant Full Disk Access in System Settings → Privacy & Security → Full Disk Access", err)
		}
		return nil, nil, fmt.Errorf("copy chat.db: %w", err)
	}
	// Copy WAL file if present so the snapshot includes recent uncommitted writes.
	if _, serr := os.Stat(a.dbPath + "-wal"); serr == nil {
		_ = copyFile(a.dbPath+"-wal", tmpDB+"-wal")
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

// ── search_messages ───────────────────────────────────────────────────────────

type messageResult struct {
	ID             string `json:"id"`
	From           string `json:"from"`
	FromIdentifier string `json:"from_identifier"`
	Text           string `json:"text"`
	Timestamp      string `json:"timestamp"`
	IsFromMe       bool   `json:"is_from_me"`
	ThreadID       string `json:"thread_id"`
}

func (a *IMessageAdapter) searchMessages(ctx context.Context, params map[string]any) (*adapters.Result, error) {
	query, _ := params["query"].(string)
	if query == "" {
		return nil, fmt.Errorf("imessage search_messages: query is required")
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

	db, cleanup, err := a.openDB()
	if err != nil {
		return nil, fmt.Errorf("imessage: cannot open chat.db: %w", err)
	}
	defer cleanup()
	defer db.Close()

	since := time.Now().Add(-time.Duration(daysBack) * 24 * time.Hour)
	// Apple uses a nanosecond epoch offset from 2001-01-01 (CoreData epoch)
	coredataEpoch := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	sinceApple := since.Sub(coredataEpoch).Nanoseconds()

	var sqlQuery string
	var args []any

	if contact != "" {
		// Resolve contact identifier first
		identifiers, _ := a.resolveContactIdentifiers(db, contact)
		if len(identifiers) == 0 {
			return &adapters.Result{
				Summary: format.Summary("No messages from %q matching %q", contact, query),
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

	rows, err := db.QueryContext(ctx, sqlQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("imessage search_messages query: %w", err)
	}
	defer rows.Close()

	msgs := a.scanMessages(rows)
	nameMap := a.buildNameMap(db, msgs)
	results := a.formatMessages(msgs, nameMap)

	return &adapters.Result{
		Summary: format.Summary("%d message(s) matching %q", len(results), query),
		Data:    results,
	}, nil
}

// ── list_threads ──────────────────────────────────────────────────────────────

type threadItem struct {
	ThreadID           string `json:"thread_id"`
	DisplayName        string `json:"display_name"`
	LastMessageSnippet string `json:"last_message_snippet"`
	LastMessageAt      string `json:"last_message_at"`
	ParticipantCount   int    `json:"participant_count"`
}

func (a *IMessageAdapter) listThreads(ctx context.Context, params map[string]any) (*adapters.Result, error) {
	maxResults := 20
	if v, ok := paramInt(params, "max_results"); ok && v > 0 && v <= 50 {
		maxResults = v
	}

	db, cleanup, err := a.openDB()
	if err != nil {
		return nil, fmt.Errorf("imessage: cannot open chat.db: %w", err)
	}
	defer cleanup()
	defer db.Close()

	rows, err := db.QueryContext(ctx, `
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
		return nil, fmt.Errorf("imessage list_threads query: %w", err)
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
			snippet = format.SanitizeText(snippet, format.MaxSnippetLen)
		}
		items = append(items, threadItem{
			ThreadID:           threadID,
			DisplayName:        format.SanitizeText(name, format.MaxFieldLen),
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
		resolved := a.resolveThreadDisplayNames(db, unresolvedIDs)
		for i := range items {
			if name, ok := resolved[items[i].ThreadID]; ok {
				items[i].DisplayName = format.SanitizeText(name, format.MaxFieldLen)
			}
		}
	}

	return &adapters.Result{
		Summary: format.Summary("%d recent conversation(s)", len(items)),
		Data:    items,
	}, nil
}

// ── get_thread ────────────────────────────────────────────────────────────────

func (a *IMessageAdapter) getThread(ctx context.Context, params map[string]any) (*adapters.Result, error) {
	contact, _ := params["contact"].(string)
	threadID, _ := params["thread_id"].(string)
	if contact == "" && threadID == "" {
		return nil, fmt.Errorf("imessage get_thread: contact or thread_id is required")
	}
	maxResults := 50
	if v, ok := paramInt(params, "max_results"); ok && v > 0 && v <= 200 {
		maxResults = v
	}
	daysBack := 30
	if v, ok := paramInt(params, "days_back"); ok && v > 0 {
		daysBack = v
	}

	db, cleanup, err := a.openDB()
	if err != nil {
		return nil, fmt.Errorf("imessage: cannot open chat.db: %w", err)
	}
	defer cleanup()
	defer db.Close()

	coredataEpoch := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	since := time.Now().Add(-time.Duration(daysBack) * 24 * time.Hour)
	sinceApple := since.Sub(coredataEpoch).Nanoseconds()

	var sqlQuery string
	var args []any

	if contact != "" {
		identifiers, _ := a.resolveContactIdentifiers(db, contact)
		if len(identifiers) == 0 {
			identifiers = []string{contact} // fallback: use value directly
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
			ORDER BY m.date ASC
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
			ORDER BY m.date ASC
			LIMIT ?`
	}

	rows, err := db.QueryContext(ctx, sqlQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("imessage get_thread query: %w", err)
	}
	defer rows.Close()

	msgs := a.scanMessages(rows)
	nameMap := a.buildNameMap(db, msgs)
	messages := a.formatMessages(msgs, nameMap)

	displayName := contact
	if displayName == "" {
		displayName = threadID
		// Resolve group chat display name from participants.
		if resolved := a.resolveThreadDisplayNames(db, []string{threadID}); resolved[threadID] != "" {
			displayName = resolved[threadID]
		}
	}
	result := map[string]any{
		"thread_id": threadID,
		"contact":   displayName,
		"messages":  messages,
	}
	return &adapters.Result{
		Summary: format.Summary("Last %d days of messages with %s (%d messages)", daysBack, displayName, len(messages)),
		Data:    result,
	}, nil
}

// ── send_message ──────────────────────────────────────────────────────────────

// SendMessage sends an iMessage via AppleScript.
// NOTE: This action ALWAYS requires human approval regardless of policy.
// The gateway handler enforces this before calling Execute.
func (a *IMessageAdapter) sendMessage(ctx context.Context, params map[string]any) (*adapters.Result, error) {
	to, _ := params["to"].(string)
	text, _ := params["text"].(string)
	if text == "" {
		// Accept "body" as an alias — Gmail and GitHub use "body", so agents
		// frequently send it for iMessage too.
		text, _ = params["body"].(string)
	}
	if to == "" {
		return nil, fmt.Errorf("imessage send_message: to is required")
	}
	if text == "" {
		return nil, fmt.Errorf("imessage send_message: text is required")
	}
	if len(text) > 2000 {
		return nil, fmt.Errorf("imessage send_message: text exceeds 2000 characters")
	}
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("imessage send_message: not supported on this platform")
	}

	// Resolve phone number / email from contact name if needed.
	identifier := to
	db, cleanup, dbErr := a.openDB()
	if dbErr == nil {
		defer cleanup()
		defer db.Close()
		identifiers, _ := a.resolveContactIdentifiers(db, to)
		if len(identifiers) > 0 {
			identifier = identifiers[0]
		}
	}

	script := fmt.Sprintf(`tell application "Messages"
	set targetService to 1st service whose service type = iMessage
	set targetBuddy to buddy %q of targetService
	send %q to targetBuddy
end tell`, identifier, text)

	cmd := exec.CommandContext(ctx, "osascript", "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("imessage send_message: AppleScript failed: %w — %s", err, truncate(string(out), 200))
	}
	return &adapters.Result{
		Summary: format.Summary("iMessage sent to %s", to),
		Data:    map[string]string{"to": to, "to_identifier": identifier},
	}, nil
}

// ── Internal helpers ──────────────────────────────────────────────────────────

type rawMessage struct {
	rowID          int64
	text           sql.NullString
	attributedBody []byte
	isFromMe       bool
	dateNS         int64
	handleID       string
	chatID         string
}

func (a *IMessageAdapter) scanMessages(rows *sql.Rows) []rawMessage {
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

func (a *IMessageAdapter) formatMessages(msgs []rawMessage, nameMap map[string]string) []messageResult {
	coredataEpoch := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	results := make([]messageResult, 0, len(msgs))
	for _, m := range msgs {
		text := ""
		if m.text.Valid {
			text = strings.TrimSpace(m.text.String)
		}
		if text == "" {
			text = extractTextFromAttributedBody(m.attributedBody)
		}
		if text == "" {
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
			Text:           format.SanitizeText(text, format.MaxBodyLen),
			Timestamp:      ts.UTC().Format(time.RFC3339),
			IsFromMe:       m.isFromMe,
			ThreadID:       m.chatID,
		})
	}
	return results
}

// buildNameMap resolves handle IDs to display names using the AddressBook DB.
func (a *IMessageAdapter) buildNameMap(db *sql.DB, msgs []rawMessage) map[string]string {
	// Collect unique non-me handle IDs.
	ids := make(map[string]bool)
	for _, m := range msgs {
		if !m.isFromMe && m.handleID != "" {
			ids[m.handleID] = true
		}
	}
	return a.lookupHandleNames(ids)
}

// lookupHandleNames resolves a set of handle IDs (phone numbers or emails) to
// display names via the macOS AddressBook database. Best-effort; returns an
// empty map on any failure.
func (a *IMessageAdapter) lookupHandleNames(handleIDs map[string]bool) map[string]string {
	nameMap := make(map[string]string)
	if len(handleIDs) == 0 {
		return nameMap
	}

	// Try to find names in AddressBook. Best-effort; silently fail.
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
		err := abDB.QueryRow(`
			SELECT p.ZFIRSTNAME, p.ZLASTNAME
			FROM ZABCDRECORD p
			JOIN ZABCDPHONENUMBER pn ON pn.ZOWNER = p.Z_PK
			WHERE pn.ZFULLNUMBER LIKE ?
			LIMIT 1`, "%"+normalizePhone(id)+"%").
			Scan(&firstName, &lastName)
		if err != nil {
			// Try email
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

// resolveThreadDisplayNames takes a chat.db handle and a list of thread IDs
// whose display names are unresolved (still raw chat_identifier), queries their
// participants, resolves names via the Address Book, and returns a map of
// threadID → "Name1, Name2, ..." (capped at 4 names).
func (a *IMessageAdapter) resolveThreadDisplayNames(db *sql.DB, threadIDs []string) map[string]string {
	if len(threadIDs) == 0 {
		return nil
	}

	// Batch-query participants for all unresolved threads.
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

	// threadID → list of handle IDs; also collect all unique handles.
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

	// Resolve all handles to names in one pass.
	nameMap := a.lookupHandleNames(allHandles)

	// Build display name per thread.
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

// resolveContactIdentifiers finds phone/email handles in chat.db matching a contact name.
func (a *IMessageAdapter) resolveContactIdentifiers(db *sql.DB, contact string) ([]string, error) {
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

	// Also look in AddressBook for the contact name → phone/email.
	abPaths, _ := filepath.Glob(filepath.Join(os.Getenv("HOME"),
		"Library/Application Support/AddressBook/Sources/*/AddressBook-v22.abcddb"))
	if len(abPaths) > 0 {
		abDB, err := sql.Open("sqlite", "file:"+abPaths[0]+"?mode=ro&immutable=1")
		if err == nil {
			defer abDB.Close()
			// Look up by first+last name.
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

// normalizePhone strips non-digit characters to normalize phone numbers.
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

// MarshalJSON is used to store empty credentials for local services.
func emptyCredential() []byte {
	b, _ := json.Marshal(map[string]string{"type": "local"})
	return b
}

// extractTextFromAttributedBody extracts plain text from the attributedBody
// BLOB column in chat.db. This column stores an NSAttributedString in Apple's
// typedstream format. In macOS 16+ the text column is often NULL and the
// actual message content is only in attributedBody.
//
// The typedstream layout for a text message is roughly:
//
//	[header "streamtyped" + version] [class hierarchy]
//	[0x2B '+' string marker] [length] [UTF-8 string bytes] [attribute data...]
//
// We locate the NSString class declaration, then scan forward for the '+'
// (0x2B) string marker followed by a length-prefixed UTF-8 string.
func extractTextFromAttributedBody(data []byte) string {
	if len(data) < 30 {
		return ""
	}

	// Find the NSString class name — the string data follows its declaration.
	idx := bytes.Index(data, []byte("NSString"))
	if idx < 0 {
		return ""
	}

	// Scan past class hierarchy for the '+' (0x2B) string marker followed
	// by a length-prefixed UTF-8 string.
	for i := idx + len("NSString"); i < len(data)-1; i++ {
		if data[i] != 0x2B {
			continue
		}

		length, skip := readTypedStreamLength(data[i+1:])
		if length <= 0 || i+1+skip+length > len(data) {
			continue
		}

		candidate := data[i+1+skip : i+1+skip+length]
		if utf8.Valid(candidate) {
			s := strings.TrimSpace(string(candidate))
			if s != "" {
				return s
			}
		}
	}
	return ""
}

// readTypedStreamLength reads a length value from Apple's typedstream format.
// Short lengths (< 128) are a single byte. For longer strings the first byte
// has the high bit set and the low 7 bits indicate how many following bytes
// encode the length in big-endian order.
func readTypedStreamLength(data []byte) (int, int) {
	if len(data) == 0 {
		return 0, 0
	}
	b := data[0]
	if b < 0x80 {
		return int(b), 1
	}
	n := int(b & 0x7F)
	if n == 0 || n > 4 || n+1 > len(data) {
		return 0, 0
	}
	length := 0
	for j := 1; j <= n; j++ {
		length = length<<8 | int(data[j])
	}
	return length, n + 1
}

func truncate(s string, max int) string {
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
