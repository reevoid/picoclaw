package wecom

import (
	"context"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
)

// ---- Webhook mode tests ----

func TestNewWeComAIBotChannel_WebhookMode(t *testing.T) {
	t.Run("success with valid config", func(t *testing.T) {
		cfg := config.WeComAIBotConfig{
			Enabled:        true,
			Token:          "test_token",
			EncodingAESKey: "testkey1234567890123456789012345678901234567",
			WebhookPath:    "/webhook/test",
		}

		messageBus := bus.NewMessageBus()
		ch, err := NewWeComAIBotChannel(cfg, messageBus)
		if err != nil {
			t.Fatalf("Expected no error, got %v", err)
		}
		if ch == nil {
			t.Fatal("Expected channel to be created")
		}
		if ch.Name() != "wecom_aibot" {
			t.Errorf("Expected name 'wecom_aibot', got '%s'", ch.Name())
		}
		// Webhook mode must implement WebhookHandler.
		if _, ok := ch.(channels.WebhookHandler); !ok {
			t.Error("Webhook mode channel should implement WebhookHandler")
		}
	})

	t.Run("error with missing token", func(t *testing.T) {
		cfg := config.WeComAIBotConfig{
			Enabled:        true,
			EncodingAESKey: "testkey1234567890123456789012345678901234567",
		}
		messageBus := bus.NewMessageBus()
		_, err := NewWeComAIBotChannel(cfg, messageBus)
		if err == nil {
			t.Fatal("Expected error for missing token, got nil")
		}
	})

	t.Run("error with missing encoding key", func(t *testing.T) {
		cfg := config.WeComAIBotConfig{
			Enabled: true,
			Token:   "test_token",
		}
		messageBus := bus.NewMessageBus()
		_, err := NewWeComAIBotChannel(cfg, messageBus)
		if err == nil {
			t.Fatal("Expected error for missing encoding key, got nil")
		}
	})
}

func TestWeComAIBotWebhookChannelStartStop(t *testing.T) {
	cfg := config.WeComAIBotConfig{
		Enabled:        true,
		Token:          "test_token",
		EncodingAESKey: "testkey1234567890123456789012345678901234567",
	}

	messageBus := bus.NewMessageBus()
	ch, err := NewWeComAIBotChannel(cfg, messageBus)
	if err != nil {
		t.Fatalf("Failed to create channel: %v", err)
	}

	ctx := context.Background()

	if err := ch.Start(ctx); err != nil {
		t.Fatalf("Failed to start channel: %v", err)
	}
	if !ch.IsRunning() {
		t.Error("Expected channel to be running after Start")
	}

	if err := ch.Stop(ctx); err != nil {
		t.Fatalf("Failed to stop channel: %v", err)
	}
	if ch.IsRunning() {
		t.Error("Expected channel to be stopped after Stop")
	}
}

func TestWeComAIBotChannelWebhookPath(t *testing.T) {
	t.Run("default path", func(t *testing.T) {
		cfg := config.WeComAIBotConfig{
			Enabled:        true,
			Token:          "test_token",
			EncodingAESKey: "testkey1234567890123456789012345678901234567",
		}
		messageBus := bus.NewMessageBus()
		ch, _ := NewWeComAIBotChannel(cfg, messageBus)

		wh, ok := ch.(channels.WebhookHandler)
		if !ok {
			t.Fatal("Expected channel to implement WebhookHandler")
		}
		expectedPath := "/webhook/wecom-aibot"
		if wh.WebhookPath() != expectedPath {
			t.Errorf("Expected webhook path '%s', got '%s'", expectedPath, wh.WebhookPath())
		}
	})

	t.Run("custom path", func(t *testing.T) {
		customPath := "/custom/webhook"
		cfg := config.WeComAIBotConfig{
			Enabled:        true,
			Token:          "test_token",
			EncodingAESKey: "testkey1234567890123456789012345678901234567",
			WebhookPath:    customPath,
		}
		messageBus := bus.NewMessageBus()
		ch, _ := NewWeComAIBotChannel(cfg, messageBus)

		wh, ok := ch.(channels.WebhookHandler)
		if !ok {
			t.Fatal("Expected channel to implement WebhookHandler")
		}
		if wh.WebhookPath() != customPath {
			t.Errorf("Expected webhook path '%s', got '%s'", customPath, wh.WebhookPath())
		}
	})
}

func TestGenerateStreamID(t *testing.T) {
	cfg := config.WeComAIBotConfig{
		Enabled:        true,
		Token:          "test_token",
		EncodingAESKey: "testkey1234567890123456789012345678901234567",
	}
	messageBus := bus.NewMessageBus()
	ch, _ := NewWeComAIBotChannel(cfg, messageBus)
	webhookCh, ok := ch.(*WeComAIBotChannel)
	if !ok {
		t.Fatal("Expected webhook mode channel")
	}

	ids := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := webhookCh.generateStreamID()
		if len(id) != 10 {
			t.Errorf("Expected stream ID length 10, got %d", len(id))
		}
		if ids[id] {
			t.Errorf("Duplicate stream ID generated: %s", id)
		}
		ids[id] = true
	}
}

func TestEncryptDecrypt(t *testing.T) {
	cfg := config.WeComAIBotConfig{
		Enabled:        true,
		Token:          "test_token",
		EncodingAESKey: "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG", // 43 characters
	}
	messageBus := bus.NewMessageBus()
	ch, _ := NewWeComAIBotChannel(cfg, messageBus)
	webhookCh, ok := ch.(*WeComAIBotChannel)
	if !ok {
		t.Fatal("Expected webhook mode channel")
	}

	plaintext := "Hello, World!"
	receiveid := ""

	encrypted, err := webhookCh.encryptMessage(plaintext, receiveid)
	if err != nil {
		t.Fatalf("Failed to encrypt message: %v", err)
	}
	if encrypted == "" {
		t.Fatal("Encrypted message is empty")
	}

	decrypted, err := decryptMessageWithVerify(encrypted, cfg.EncodingAESKey, receiveid)
	if err != nil {
		t.Fatalf("Failed to decrypt message: %v", err)
	}
	if decrypted != plaintext {
		t.Errorf("Expected decrypted message '%s', got '%s'", plaintext, decrypted)
	}
}

func TestGenerateSignature(t *testing.T) {
	token := "test_token"
	timestamp := "1234567890"
	nonce := "test_nonce"
	encrypt := "encrypted_msg"

	signature := computeSignature(token, timestamp, nonce, encrypt)
	if signature == "" {
		t.Error("Generated signature is empty")
	}
	if !verifySignature(token, signature, timestamp, nonce, encrypt) {
		t.Error("Generated signature does not verify correctly")
	}
}

// ---- WebSocket long-connection mode tests ----

func TestNewWeComAIBotChannel_WSMode(t *testing.T) {
	t.Run("success with bot_id and secret", func(t *testing.T) {
		cfg := config.WeComAIBotConfig{
			Enabled: true,
			BotID:   "test_bot_id",
			Secret:  "test_secret",
		}
		messageBus := bus.NewMessageBus()
		ch, err := NewWeComAIBotChannel(cfg, messageBus)
		if err != nil {
			t.Fatalf("Expected no error, got %v", err)
		}
		if ch == nil {
			t.Fatal("Expected channel to be created")
		}
		if ch.Name() != "wecom_aibot" {
			t.Errorf("Expected name 'wecom_aibot', got '%s'", ch.Name())
		}
		// WebSocket mode must NOT implement WebhookHandler.
		if _, ok := ch.(channels.WebhookHandler); ok {
			t.Error("WebSocket mode channel should NOT implement WebhookHandler")
		}
	})

	t.Run("ws mode takes priority over webhook fields", func(t *testing.T) {
		cfg := config.WeComAIBotConfig{
			Enabled:        true,
			BotID:          "test_bot_id",
			Secret:         "test_secret",
			Token:          "also_set",
			EncodingAESKey: "testkey1234567890123456789012345678901234567",
		}
		messageBus := bus.NewMessageBus()
		ch, err := NewWeComAIBotChannel(cfg, messageBus)
		if err != nil {
			t.Fatalf("Expected no error, got %v", err)
		}
		if _, ok := ch.(*WeComAIBotWSChannel); !ok {
			t.Error("Expected WebSocket mode channel when both BotID+Secret and Token+Key are set")
		}
	})

	t.Run("error with missing bot_id", func(t *testing.T) {
		cfg := config.WeComAIBotConfig{
			Enabled: true,
			Secret:  "test_secret",
		}
		messageBus := bus.NewMessageBus()
		_, err := NewWeComAIBotChannel(cfg, messageBus)
		// Missing bot_id alone means neither WS mode nor webhook mode is fully configured.
		if err == nil {
			t.Fatal("Expected error for missing bot_id, got nil")
		}
	})

	t.Run("error with missing secret", func(t *testing.T) {
		cfg := config.WeComAIBotConfig{
			Enabled: true,
			BotID:   "test_bot_id",
		}
		messageBus := bus.NewMessageBus()
		_, err := NewWeComAIBotChannel(cfg, messageBus)
		if err == nil {
			t.Fatal("Expected error for missing secret, got nil")
		}
	})
}

func TestWeComAIBotWSChannelStartStop(t *testing.T) {
	cfg := config.WeComAIBotConfig{
		Enabled: true,
		BotID:   "test_bot_id",
		Secret:  "test_secret",
	}
	messageBus := bus.NewMessageBus()
	ch, err := NewWeComAIBotChannel(cfg, messageBus)
	if err != nil {
		t.Fatalf("Failed to create channel: %v", err)
	}

	ctx := context.Background()

	// Start launches a background goroutine; it should not block or return an error.
	if err := ch.Start(ctx); err != nil {
		t.Fatalf("Failed to start channel: %v", err)
	}
	if !ch.IsRunning() {
		t.Error("Expected channel to be running after Start")
	}

	// Stop should work regardless of whether the WebSocket actually connected.
	if err := ch.Stop(ctx); err != nil {
		t.Fatalf("Failed to stop channel: %v", err)
	}
	if ch.IsRunning() {
		t.Error("Expected channel to be stopped after Stop")
	}
}

func TestGenerateRandomID(t *testing.T) {
	ids := make(map[string]bool)
	for i := 0; i < 200; i++ {
		id := generateRandomID(10)
		if len(id) != 10 {
			t.Errorf("Expected ID length 10, got %d", len(id))
		}
		if ids[id] {
			t.Errorf("Duplicate ID generated: %s", id)
		}
		ids[id] = true
	}
}

func TestWSGenerateID(t *testing.T) {
	ids := make(map[string]bool)
	for i := 0; i < 200; i++ {
		id := wsGenerateID()
		if len(id) != 10 {
			t.Errorf("Expected ID length 10, got %d", len(id))
		}
		if ids[id] {
			t.Errorf("Duplicate wsGenerateID result: %s", id)
		}
		ids[id] = true
	}
}
