package wecom

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/identity"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/utils"
)

// Long-connection WebSocket endpoint.
// Ref: https://developer.work.weixin.qq.com/document/path/101463
const (
	wsEndpoint          = "wss://openws.work.weixin.qq.com"
	wsHeartbeatInterval = 30 * time.Second
	wsConnectTimeout    = 15 * time.Second
	wsSubscribeTimeout  = 10 * time.Second
	wsMaxReconnectWait  = 60 * time.Second
	wsInitialReconnect  = time.Second

	// WeCom requires finish=true within 6 minutes of the first stream frame.
	// wsStreamTickInterval controls how often we send an in-progress hint.
	// wsStreamMaxDuration is a safety margin below the 6-minute hard limit.
	wsStreamTickInterval = 30 * time.Second
	wsStreamMaxDuration  = 5*time.Minute + 30*time.Second
)

// WeComAIBotWSChannel implements channels.Channel for WeCom AI Bot using the
// WebSocket long-connection API.
// Unlike the webhook counterpart it does NOT implement WebhookHandler, so the
// HTTP manager will not register any callback URL for it.
type WeComAIBotWSChannel struct {
	*channels.BaseChannel
	config config.WeComAIBotConfig
	ctx    context.Context
	cancel context.CancelFunc

	// conn is the active WebSocket connection; nil when disconnected.
	// All writes are serialized through connMu.
	conn   *websocket.Conn
	connMu sync.Mutex

	// tasks holds one live agent task per chatID.
	// A new message for the same chat cancels the previous task.
	tasks   map[string]*wsTask
	tasksMu sync.Mutex

	// reqPending correlates command req_ids with response channels.
	// Used only for subscribe/ping command-response pairs.
	reqPending   map[string]chan wsEnvelope
	reqPendingMu sync.Mutex
}

// wsTask tracks one in-progress agent reply for a single chat turn.
type wsTask struct {
	ReqID       string // req_id echoed in all replies for this turn
	ChatID      string
	StreamID    string // our generated stream.id
	CreatedTime time.Time
	answerCh    chan string // agent delivers its reply here via Send()
	ctx         context.Context
	cancel      context.CancelFunc
}

// ---- WebSocket protocol types ----

// wsEnvelope is the generic JSON envelope for all WebSocket messages.
type wsEnvelope struct {
	Cmd     string          `json:"cmd,omitempty"`
	Headers wsHeaders       `json:"headers"`
	Body    json.RawMessage `json:"body,omitempty"`
	ErrCode int             `json:"errcode,omitempty"`
	ErrMsg  string          `json:"errmsg,omitempty"`
}

type wsHeaders struct {
	ReqID string `json:"req_id"`
}

// wsCommand is an outgoing request sent over the WebSocket.
type wsCommand struct {
	Cmd     string    `json:"cmd"`
	Headers wsHeaders `json:"headers"`
	Body    any       `json:"body,omitempty"`
}

// wsRespondMsgBody is the body for aibot_respond_msg / aibot_respond_welcome_msg.
type wsRespondMsgBody struct {
	MsgType  string             `json:"msgtype"`
	Stream   *wsStreamContent   `json:"stream,omitempty"`
	Text     *wsTextContent     `json:"text,omitempty"`
	Markdown *wsMarkdownContent `json:"markdown,omitempty"`
}

type wsStreamContent struct {
	ID      string `json:"id"`
	Finish  bool   `json:"finish"`
	Content string `json:"content,omitempty"`
}

type wsTextContent struct {
	Content string `json:"content"`
}

type wsMarkdownContent struct {
	Content string `json:"content"`
}

// WeComAIBotWSMessage is the decoded body of aibot_msg_callback /
// aibot_event_callback in WebSocket long-connection mode.
// The structure mirrors WeComAIBotMessage but includes extra fields
// that only appear in long-connection callbacks (Voice, AESKey on Image/File).
type WeComAIBotWSMessage struct {
	MsgID      string `json:"msgid"`
	CreateTime int64  `json:"create_time,omitempty"`
	AIBotID    string `json:"aibotid"`
	ChatID     string `json:"chatid,omitempty"`
	ChatType   string `json:"chattype,omitempty"` // "single" | "group"
	From       struct {
		UserID string `json:"userid"`
	} `json:"from"`
	MsgType string `json:"msgtype"`
	Text    *struct {
		Content string `json:"content"`
	} `json:"text,omitempty"`
	Image *struct {
		URL    string `json:"url"`
		AESKey string `json:"aeskey,omitempty"` // long-connection: per-resource decrypt key
	} `json:"image,omitempty"`
	Voice *struct {
		Text string `json:"text"` // WeCom transcribes voice to text in callbacks
	} `json:"voice,omitempty"`
	Mixed *struct {
		MsgItem []struct {
			MsgType string `json:"msgtype"`
			Text    *struct {
				Content string `json:"content"`
			} `json:"text,omitempty"`
			Image *struct {
				URL    string `json:"url"`
				AESKey string `json:"aeskey,omitempty"`
			} `json:"image,omitempty"`
		} `json:"msg_item"`
	} `json:"mixed,omitempty"`
	Event *struct {
		EventType string `json:"eventtype"`
	} `json:"event,omitempty"`
}

// ---- Constructor ----

// newWeComAIBotWSChannel creates a WeComAIBotWSChannel for WebSocket mode.
func newWeComAIBotWSChannel(
	cfg config.WeComAIBotConfig,
	messageBus *bus.MessageBus,
) (*WeComAIBotWSChannel, error) {
	if cfg.BotID == "" || cfg.Secret == "" {
		return nil, fmt.Errorf("bot_id and secret are required for WeCom AI Bot WebSocket mode")
	}

	base := channels.NewBaseChannel("wecom_aibot", cfg, messageBus, cfg.AllowFrom,
		channels.WithMaxMessageLength(2048),
		channels.WithReasoningChannelID(cfg.ReasoningChannelID),
	)

	return &WeComAIBotWSChannel{
		BaseChannel: base,
		config:      cfg,
		tasks:       make(map[string]*wsTask),
		reqPending:  make(map[string]chan wsEnvelope),
	}, nil
}

// ---- Channel interface ----

// Name implements channels.Channel.
func (c *WeComAIBotWSChannel) Name() string { return "wecom_aibot" }

// Start connects to the WeCom WebSocket endpoint and begins message processing.
func (c *WeComAIBotWSChannel) Start(ctx context.Context) error {
	logger.InfoC("wecom_aibot", "Starting WeCom AI Bot channel (WebSocket long-connection mode)...")
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.SetRunning(true)
	go c.connectLoop()
	logger.InfoC("wecom_aibot", "WeCom AI Bot channel started (WebSocket mode)")
	return nil
}

// Stop shuts down the channel and closes the WebSocket connection.
func (c *WeComAIBotWSChannel) Stop(_ context.Context) error {
	logger.InfoC("wecom_aibot", "Stopping WeCom AI Bot channel (WebSocket mode)...")
	if c.cancel != nil {
		c.cancel()
	}
	c.connMu.Lock()
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
	c.connMu.Unlock()
	c.SetRunning(false)
	logger.InfoC("wecom_aibot", "WeCom AI Bot channel stopped")
	return nil
}

// Send delivers the agent reply for msg.ChatID.
// The waiting task goroutine picks it up and writes the final stream response.
func (c *WeComAIBotWSChannel) Send(ctx context.Context, msg bus.OutboundMessage) error {
	if !c.IsRunning() {
		return channels.ErrNotRunning
	}

	c.tasksMu.Lock()
	task := c.tasks[msg.ChatID]
	c.tasksMu.Unlock()

	if task == nil {
		logger.DebugCF("wecom_aibot", "Send: no active task for chat (may have finished or timed out)",
			map[string]any{"chat_id": msg.ChatID})
		return nil
	}

	select {
	case task.answerCh <- msg.Content:
	case <-task.ctx.Done():
		return nil // task canceled (connection dropped)
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// ---- Connection management ----

// connectLoop maintains the WebSocket connection, reconnecting on failure with
// exponential backoff.
func (c *WeComAIBotWSChannel) connectLoop() {
	backoff := wsInitialReconnect
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		logger.InfoC("wecom_aibot", "Connecting to WeCom WebSocket endpoint...")
		if err := c.runConnection(); err != nil {
			select {
			case <-c.ctx.Done():
				return
			default:
				logger.WarnCF("wecom_aibot", "WebSocket connection lost, reconnecting",
					map[string]any{"error": err, "backoff": backoff.String()})
				select {
				case <-time.After(backoff):
				case <-c.ctx.Done():
					return
				}
				if backoff < wsMaxReconnectWait {
					backoff *= 2
					if backoff > wsMaxReconnectWait {
						backoff = wsMaxReconnectWait
					}
				}
			}
		} else {
			// Clean exit (context canceled); stop reconnecting.
			return
		}
	}
}

// runConnection dials, subscribes, and runs the read/heartbeat loops until the
// connection closes or the channel context is canceled.
func (c *WeComAIBotWSChannel) runConnection() error {
	dialCtx, dialCancel := context.WithTimeout(c.ctx, wsConnectTimeout)
	conn, httpResp, err := websocket.DefaultDialer.DialContext(dialCtx, wsEndpoint, nil)
	dialCancel()
	if httpResp != nil {
		httpResp.Body.Close()
	}
	if err != nil {
		return fmt.Errorf("dial failed: %w", err)
	}

	c.connMu.Lock()
	c.conn = conn
	c.connMu.Unlock()

	defer func() {
		c.connMu.Lock()
		if c.conn == conn {
			c.conn = nil
		}
		c.connMu.Unlock()
		// Cancel any tasks that were started over this connection so their
		// agent goroutines do not keep running after the connection is gone.
		c.cancelAllTasks()
	}()

	// ---- Read loop (must start BEFORE subscribing) ----
	// sendAndWait blocks waiting for the subscribe response on reqPending;
	// readLoop is the only goroutine that delivers messages to reqPending.
	// Starting readLoop first avoids a deadlock where sendAndWait times out
	// because no one reads the server's reply.
	readErrCh := make(chan error, 1)
	go func() { readErrCh <- c.readLoop(conn) }()

	// ---- Subscribe ----
	reqID := wsGenerateID()
	resp, err := c.sendAndWait(conn, reqID, wsCommand{
		Cmd:     "aibot_subscribe",
		Headers: wsHeaders{ReqID: reqID},
		Body: map[string]string{
			"bot_id": c.config.BotID,
			"secret": c.config.Secret,
		},
	}, wsSubscribeTimeout)
	if err != nil {
		conn.Close() // stop readLoop
		<-readErrCh
		return fmt.Errorf("subscribe failed: %w", err)
	}
	if resp.ErrCode != 0 {
		conn.Close()
		<-readErrCh
		return fmt.Errorf("subscribe rejected (errcode=%d): %s", resp.ErrCode, resp.ErrMsg)
	}

	logger.InfoC("wecom_aibot", "WebSocket subscription successful")

	// ---- Heartbeat goroutine ----
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		c.heartbeatLoop(conn)
	}()

	// Wait for the read loop to exit, then tear down the heartbeat.
	readErr := <-readErrCh
	conn.Close() // signal heartbeat to stop (idempotent)
	<-hbDone
	return readErr
}

// sendAndWait registers a pending-response slot, sends cmd, and blocks until
// the matching response arrives or the timeout/context fires.
func (c *WeComAIBotWSChannel) sendAndWait(
	conn *websocket.Conn,
	reqID string,
	cmd wsCommand,
	timeout time.Duration,
) (wsEnvelope, error) {
	ch := make(chan wsEnvelope, 1)
	c.reqPendingMu.Lock()
	c.reqPending[reqID] = ch
	c.reqPendingMu.Unlock()

	cleanup := func() {
		c.reqPendingMu.Lock()
		delete(c.reqPending, reqID)
		c.reqPendingMu.Unlock()
	}

	data, err := json.Marshal(cmd)
	if err != nil {
		cleanup()
		return wsEnvelope{}, fmt.Errorf("marshal command: %w", err)
	}
	c.connMu.Lock()
	err = conn.WriteMessage(websocket.TextMessage, data)
	c.connMu.Unlock()
	if err != nil {
		cleanup()
		return wsEnvelope{}, fmt.Errorf("write command: %w", err)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case env := <-ch:
		return env, nil
	case <-timer.C:
		cleanup()
		return wsEnvelope{}, fmt.Errorf("timeout waiting for response (req_id=%s)", reqID)
	case <-c.ctx.Done():
		cleanup()
		return wsEnvelope{}, c.ctx.Err()
	}
}

// heartbeatLoop sends a ping every wsHeartbeatInterval until conn is closed.
func (c *WeComAIBotWSChannel) heartbeatLoop(conn *websocket.Conn) {
	ticker := time.NewTicker(wsHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			reqID := wsGenerateID()
			data, _ := json.Marshal(wsCommand{
				Cmd:     "ping",
				Headers: wsHeaders{ReqID: reqID},
			})
			c.connMu.Lock()
			err := conn.WriteMessage(websocket.TextMessage, data)
			c.connMu.Unlock()
			if err != nil {
				logger.WarnCF("wecom_aibot", "Heartbeat write failed", map[string]any{"error": err})
				return
			}
			logger.DebugCF("wecom_aibot", "Heartbeat sent", map[string]any{"req_id": reqID})
		case <-c.ctx.Done():
			return
		}
	}
}

// readLoop reads WebSocket messages and dispatches them until the connection
// closes or the channel is stopped.
func (c *WeComAIBotWSChannel) readLoop(conn *websocket.Conn) error {
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			select {
			case <-c.ctx.Done():
				return nil // clean shutdown
			default:
				return fmt.Errorf("read error: %w", err)
			}
		}

		var env wsEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			logger.WarnCF("wecom_aibot", "Failed to parse WebSocket message",
				map[string]any{"error": err, "raw": string(raw)})
			continue
		}

		// If there is a waiting sendAndWait() call for this req_id, forward
		// the envelope to it.  Command responses have an empty Cmd field.
		if env.Cmd == "" && env.Headers.ReqID != "" {
			c.reqPendingMu.Lock()
			ch, ok := c.reqPending[env.Headers.ReqID]
			if ok {
				delete(c.reqPending, env.Headers.ReqID)
			}
			c.reqPendingMu.Unlock()
			if ok {
				ch <- env
				continue
			}
		}

		// Dispatch to appropriate handler in a separate goroutine so the
		// read loop is never blocked by a slow agent.
		go c.handleEnvelope(env)
	}
}

// ---- Message / event handlers ----

// handleEnvelope routes a WebSocket envelope to the right handler.
func (c *WeComAIBotWSChannel) handleEnvelope(env wsEnvelope) {
	switch env.Cmd {
	case "aibot_msg_callback":
		c.handleMsgCallback(env)
	case "aibot_event_callback":
		c.handleEventCallback(env)
	default:
		logger.DebugCF("wecom_aibot", "Unhandled WebSocket command",
			map[string]any{"cmd": env.Cmd})
	}
}

// handleMsgCallback processes aibot_msg_callback.
func (c *WeComAIBotWSChannel) handleMsgCallback(env wsEnvelope) {
	var msg WeComAIBotWSMessage
	if err := json.Unmarshal(env.Body, &msg); err != nil {
		logger.WarnCF("wecom_aibot", "Failed to parse msg callback body",
			map[string]any{"error": err})
		return
	}

	reqID := env.Headers.ReqID
	switch msg.MsgType {
	case "text":
		c.handleWSTextMessage(reqID, msg)
	case "image":
		c.handleWSImageMessage(reqID, msg)
	case "voice":
		c.handleWSVoiceMessage(reqID, msg)
	case "mixed":
		c.handleWSMixedMessage(reqID, msg)
	default:
		logger.WarnCF("wecom_aibot", "Unsupported message type",
			map[string]any{"msgtype": msg.MsgType})
		c.wsSendStreamFinish(reqID, wsGenerateID(),
			"Unsupported message type: "+msg.MsgType)
	}
}

// handleEventCallback processes aibot_event_callback.
func (c *WeComAIBotWSChannel) handleEventCallback(env wsEnvelope) {
	var msg WeComAIBotWSMessage
	if err := json.Unmarshal(env.Body, &msg); err != nil {
		logger.WarnCF("wecom_aibot", "Failed to parse event callback body",
			map[string]any{"error": err})
		return
	}

	var eventType string
	if msg.Event != nil {
		eventType = msg.Event.EventType
	}
	logger.DebugCF("wecom_aibot", "Received event callback",
		map[string]any{"event_type": eventType})

	switch eventType {
	case "enter_chat":
		if c.config.WelcomeMessage != "" {
			c.wsSendWelcomeMsg(env.Headers.ReqID, c.config.WelcomeMessage)
		}
	case "disconnected_event":
		// The server will close this connection after sending this event.
		// connectLoop will detect the closure and reconnect automatically.
		logger.WarnC("wecom_aibot",
			"Received disconnected_event: this connection is being replaced by a newer one")
	default:
		logger.DebugCF("wecom_aibot", "Unhandled event type",
			map[string]any{"event_type": eventType})
	}
}

// handleWSTextMessage dispatches a plain-text message to the agent and streams
// the reply back over the WebSocket connection.
func (c *WeComAIBotWSChannel) handleWSTextMessage(reqID string, msg WeComAIBotWSMessage) {
	if msg.Text == nil {
		logger.ErrorC("wecom_aibot", "text message missing text field")
		return
	}

	userID := msg.From.UserID
	if userID == "" {
		userID = "unknown"
	}
	chatID := msg.ChatID
	if chatID == "" {
		chatID = userID
	}

	streamID := wsGenerateID()
	taskCtx, taskCancel := context.WithCancel(c.ctx)

	task := &wsTask{
		ReqID:       reqID,
		ChatID:      chatID,
		StreamID:    streamID,
		CreatedTime: time.Now(),
		answerCh:    make(chan string, 1),
		ctx:         taskCtx,
		cancel:      taskCancel,
	}

	c.tasksMu.Lock()
	// Cancel any previous task for this chat (user sent a new message mid-reply).
	if prev, ok := c.tasks[chatID]; ok {
		prev.cancel()
	}
	c.tasks[chatID] = task
	c.tasksMu.Unlock()

	// Send an empty stream opening frame (finish=false) immediately so WeCom
	// shows the typing indicator while the agent is processing.
	c.wsSendStreamChunk(reqID, streamID, false, "")

	go func() {
		defer func() {
			taskCancel()
			c.tasksMu.Lock()
			if c.tasks[chatID] == task {
				delete(c.tasks, chatID)
			}
			c.tasksMu.Unlock()
		}()

		sender := bus.SenderInfo{
			Platform:    "wecom_aibot",
			PlatformID:  userID,
			CanonicalID: identity.BuildCanonicalID("wecom_aibot", userID),
			DisplayName: userID,
		}
		peerKind := "direct"
		if msg.ChatType == "group" {
			peerKind = "group"
		}
		peer := bus.Peer{Kind: peerKind, ID: chatID}
		metadata := map[string]string{
			"channel":   "wecom_aibot",
			"chat_type": msg.ChatType,
			"msg_type":  "text",
			"msgid":     msg.MsgID,
			"aibotid":   msg.AIBotID,
			"stream_id": streamID,
		}
		// PublishInbound is non-blocking; the agent will call Send() when done.
		c.HandleMessage(taskCtx, peer, msg.MsgID, userID, chatID,
			msg.Text.Content, nil, metadata, sender)

		// Wait for the agent reply. While waiting, send periodic finish=false
		// hints so the user knows processing is still in progress.
		// WeCom requires finish=true within 6 minutes of the first stream frame;
		// wsStreamMaxDuration enforces that limit with a safety margin.
		waitHints := []string{
			"⏳ Processing, please wait...",
			"⏳ Still processing, please wait...",
			"⏳ Almost there, please wait...",
		}
		ticker := time.NewTicker(wsStreamTickInterval)
		defer ticker.Stop()
		deadlineTimer := time.NewTimer(wsStreamMaxDuration)
		defer deadlineTimer.Stop()
		tickCount := 0
		for {
			select {
			case answer := <-task.answerCh:
				// Agent replied — deliver the real answer as the final frame.
				c.wsSendStreamFinish(reqID, streamID, answer)
				return
			case <-ticker.C:
				// Send an in-progress hint (finish=false). WeCom replaces the
				// displayed content with the latest non-finish frame, so the
				// user just sees the most recent hint, not an accumulation.
				hint := waitHints[tickCount%len(waitHints)]
				tickCount++
				logger.DebugCF("wecom_aibot", "Sending stream progress hint",
					map[string]any{"chat_id": chatID, "tick": tickCount})
				c.wsSendStreamChunk(reqID, streamID, false, hint)
			case <-deadlineTimer.C:
				// Hard deadline reached before the agent replied. Close the
				// stream with a timeout notice (still within the 6-minute window).
				logger.WarnCF("wecom_aibot", "Stream deadline reached without agent reply",
					map[string]any{"chat_id": chatID, "stream_id": streamID})
				c.wsSendStreamFinish(reqID, streamID,
					"⏳ Processing is taking longer than expected. Please resend your message to try again.")
				return
			case <-taskCtx.Done():
				// Connection dropped or task canceled; nothing to send.
				return
			}
		}
	}()
}

// handleWSImageMessage handles image messages.
func (c *WeComAIBotWSChannel) handleWSImageMessage(reqID string, msg WeComAIBotWSMessage) {
	logger.WarnC("wecom_aibot", "Image messages not yet supported in WebSocket mode")
	content := "Image messages are not yet supported."
	if msg.Image != nil {
		content = fmt.Sprintf(
			"Image received (URL: %s), but image messages are not yet supported.",
			msg.Image.URL,
		)
	}
	c.wsSendStreamFinish(reqID, wsGenerateID(), content)
}

// handleWSMixedMessage handles mixed text+image messages.
// If the message contains a text part it is handled as a text message;
// otherwise an unsupported-type notice is returned.
func (c *WeComAIBotWSChannel) handleWSMixedMessage(reqID string, msg WeComAIBotWSMessage) {
	if msg.Mixed != nil {
		for _, item := range msg.Mixed.MsgItem {
			if item.MsgType == "text" && item.Text != nil {
				// Treat the text portion as a standalone text message.
				c.handleWSTextMessage(reqID, WeComAIBotWSMessage{
					MsgID:    msg.MsgID,
					AIBotID:  msg.AIBotID,
					ChatID:   msg.ChatID,
					ChatType: msg.ChatType,
					From:     msg.From,
					MsgType:  "text",
					Text:     item.Text,
				})
				return
			}
		}
	}
	logger.WarnC("wecom_aibot", "Mixed message has no usable text part")
	c.wsSendStreamFinish(reqID, wsGenerateID(), "Mixed message type is not yet fully supported.")
}

// handleWSVoiceMessage handles voice messages.
// WeCom transcribes voice to text in the callback; if the transcription is
// present it is forwarded as a text message.
func (c *WeComAIBotWSChannel) handleWSVoiceMessage(reqID string, msg WeComAIBotWSMessage) {
	if msg.Voice != nil && msg.Voice.Text != "" {
		c.handleWSTextMessage(reqID, WeComAIBotWSMessage{
			MsgID:    msg.MsgID,
			AIBotID:  msg.AIBotID,
			ChatID:   msg.ChatID,
			ChatType: msg.ChatType,
			From:     msg.From,
			MsgType:  "text",
			Text: &struct {
				Content string `json:"content"`
			}{Content: msg.Voice.Text},
		})
		return
	}
	c.wsSendStreamFinish(reqID, wsGenerateID(), "Voice messages are not yet supported.")
}

// ---- WebSocket write helpers ----

// wsSendStreamChunk sends an aibot_respond_msg stream frame.
func (c *WeComAIBotWSChannel) wsSendStreamChunk(reqID, streamID string, finish bool, content string) {
	logger.DebugCF("wecom_aibot", "Sending stream chunk", map[string]any{
		"stream_id": streamID,
		"finish":    finish,
		"preview":   utils.Truncate(content, 100),
	})
	c.writeWS(wsCommand{
		Cmd:     "aibot_respond_msg",
		Headers: wsHeaders{ReqID: reqID},
		Body: wsRespondMsgBody{
			MsgType: "stream",
			Stream: &wsStreamContent{
				ID:      streamID,
				Finish:  finish,
				Content: content,
			},
		},
	})
}

// wsSendStreamFinish sends the final aibot_respond_msg frame (finish=true).
func (c *WeComAIBotWSChannel) wsSendStreamFinish(reqID, streamID, content string) {
	c.wsSendStreamChunk(reqID, streamID, true, content)
}

// wsSendWelcomeMsg sends a text welcome message via aibot_respond_welcome_msg.
func (c *WeComAIBotWSChannel) wsSendWelcomeMsg(reqID, content string) {
	logger.DebugCF("wecom_aibot", "Sending welcome message", map[string]any{"req_id": reqID})
	c.writeWS(wsCommand{
		Cmd:     "aibot_respond_welcome_msg",
		Headers: wsHeaders{ReqID: reqID},
		Body: wsRespondMsgBody{
			MsgType: "text",
			Text:    &wsTextContent{Content: content},
		},
	})
}

// writeWS serializes cmd to JSON and writes it to the active WebSocket
// connection.  It is safe to call from multiple goroutines.
func (c *WeComAIBotWSChannel) writeWS(cmd any) {
	data, err := json.Marshal(cmd)
	if err != nil {
		logger.ErrorCF("wecom_aibot", "Failed to marshal WebSocket command",
			map[string]any{"error": err})
		return
	}
	c.connMu.Lock()
	conn := c.conn
	if conn != nil {
		err = conn.WriteMessage(websocket.TextMessage, data)
	}
	c.connMu.Unlock()
	if conn == nil {
		logger.WarnC("wecom_aibot", "WebSocket connection unavailable, dropping outbound message")
		return
	}
	if err != nil {
		logger.WarnCF("wecom_aibot", "WebSocket write failed", map[string]any{"error": err})
	}
}

// cancelAllTasks cancels every pending agent task; called when the connection drops.
func (c *WeComAIBotWSChannel) cancelAllTasks() {
	c.tasksMu.Lock()
	defer c.tasksMu.Unlock()
	for chatID, task := range c.tasks {
		task.cancel()
		delete(c.tasks, chatID)
	}
}

// wsGenerateID generates a random 10-character alphanumeric ID.
// It is package-level (not a method) so it can be shared by both channel modes.
func wsGenerateID() string {
	return generateRandomID(10)
}
