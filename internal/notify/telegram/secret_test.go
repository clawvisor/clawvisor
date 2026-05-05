package telegram

import (
	"context"
	"strings"
	"testing"

	sqlitestore "github.com/clawvisor/clawvisor/pkg/store/sqlite"
	intvault "github.com/clawvisor/clawvisor/pkg/vault"
)

// TestSaveTelegramConfig_EncryptsBotToken verifies that when a vault is
// configured, SaveTelegramConfig writes the bot token to the vault and
// leaves the notification_configs.config JSON column free of any plaintext
// copy. This is the regression guard for the prior bug where the bot token
// was stored unencrypted in a database column.
func TestSaveTelegramConfig_EncryptsBotToken(t *testing.T) {
	ctx := context.Background()

	db, err := sqlitestore.New(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	st := sqlitestore.NewStore(db)
	user, err := st.CreateUser(ctx, "encrypt@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	v, err := intvault.NewLocalVault(t.TempDir()+"/vault.key", db, "sqlite")
	if err != nil {
		t.Fatalf("vault: %v", err)
	}

	n := New(st, ctx)
	n.SetVault(v)

	const botToken = "1234567:VERY-SECRET-TOKEN"
	const chatID = "99999"

	if err := n.SaveTelegramConfig(ctx, user.ID, botToken, chatID); err != nil {
		t.Fatalf("SaveTelegramConfig: %v", err)
	}

	// 1. The notification_configs row exists, contains the chat_id, and does
	//    not contain the bot token in any form.
	nc, err := st.GetNotificationConfig(ctx, user.ID, "telegram")
	if err != nil {
		t.Fatalf("GetNotificationConfig: %v", err)
	}
	raw := string(nc.Config)
	if !strings.Contains(raw, `"chat_id":"`+chatID+`"`) {
		t.Errorf("expected chat_id in config row, got %q", raw)
	}
	if strings.Contains(raw, botToken) {
		t.Fatalf("bot token leaked into notification_configs.config: %q", raw)
	}
	if strings.Contains(raw, "VERY-SECRET-TOKEN") {
		t.Fatalf("bot token substring leaked into notification_configs.config: %q", raw)
	}

	// 2. The vault holds the encrypted bot token and decrypts it back.
	stored, err := v.Get(ctx, user.ID, vaultBotTokenKey)
	if err != nil {
		t.Fatalf("vault.Get: %v", err)
	}
	if string(stored) != botToken {
		t.Fatalf("vault round-trip mismatch: got %q want %q", stored, botToken)
	}

	// 3. userConfig pulls from the vault and reassembles the original config.
	gotToken, gotChat, err := n.userConfig(ctx, user.ID)
	if err != nil {
		t.Fatalf("userConfig: %v", err)
	}
	if gotToken != botToken || gotChat != chatID {
		t.Fatalf("userConfig mismatch: got (%q, %q) want (%q, %q)", gotToken, gotChat, botToken, chatID)
	}

	// 4. Delete clears both the row and the vault entry.
	if err := n.DeleteTelegramConfig(ctx, user.ID); err != nil {
		t.Fatalf("DeleteTelegramConfig: %v", err)
	}
	if _, err := st.GetNotificationConfig(ctx, user.ID, "telegram"); err == nil {
		t.Fatalf("expected notification config to be deleted")
	}
	if _, err := v.Get(ctx, user.ID, vaultBotTokenKey); err == nil {
		t.Fatalf("expected vault entry to be deleted")
	}
}

// TestUserConfig_LegacyPlaintextRowFallback proves that pre-encryption rows
// (where bot_token sits inside the JSON column) still resolve correctly so
// upgrades don't lock users out. Once SaveTelegramConfig is called again,
// the next read pulls from the vault and the JSON column should no longer
// be required.
func TestUserConfig_LegacyPlaintextRowFallback(t *testing.T) {
	ctx := context.Background()

	db, err := sqlitestore.New(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	st := sqlitestore.NewStore(db)
	user, err := st.CreateUser(ctx, "legacy@test.example", "hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	v, err := intvault.NewLocalVault(t.TempDir()+"/vault.key", db, "sqlite")
	if err != nil {
		t.Fatalf("vault: %v", err)
	}

	// Simulate a pre-fix row: token sits in the JSON column with no vault entry.
	if err := st.UpsertNotificationConfig(ctx, user.ID, "telegram",
		[]byte(`{"bot_token":"legacy-token","chat_id":"7"}`)); err != nil {
		t.Fatalf("UpsertNotificationConfig: %v", err)
	}

	n := New(st, ctx)
	n.SetVault(v)

	tok, chat, err := n.userConfig(ctx, user.ID)
	if err != nil {
		t.Fatalf("userConfig (legacy fallback): %v", err)
	}
	if tok != "legacy-token" || chat != "7" {
		t.Fatalf("legacy fallback mismatch: got (%q, %q)", tok, chat)
	}
}
