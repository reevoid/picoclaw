package wecom

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/identity"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/media"
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

	// wsImageDownloadTimeout caps the time we spend downloading an inbound image.
	wsImageDownloadTimeout = 30 * time.Second

	// wsMediaWaitTimeout is how long the goroutine waits for images (delivered via
	// SendMedia → mediaCh) AFTER sending the text finish frame, before giving up.
	wsMediaWaitTimeout = 500 * time.Millisecond
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

	// lastReqIDs records the most recent req_id per chat so that SendMedia can
	// send images even after the stream has finished.
	lastReqIDs   map[string]wsReqIDRecord
	lastReqIDsMu sync.Mutex

	// tasksByMsgID allows Send() to route a response to the exact task that
	// originated from a given inbound message ID, even if a newer task has
	// since replaced it in the tasks map (concurrent-message race prevention).
	// Protected by tasksMu.
	tasksByMsgID map[string]*wsTask
}

// wsReqIDRecord stores a req_id and its expiry time.
type wsReqIDRecord struct {
	ReqID     string
	ExpiresAt time.Time
}

// wsTask tracks one in-progress agent reply for a single chat turn.
type wsTask struct {
	ReqID       string // req_id echoed in all replies for this turn
	ChatID      string
	StreamID    string // our generated stream.id
	CreatedTime time.Time
	answerCh    chan string          // agent delivers its reply here via Send()
	mediaCh     chan []bus.MediaPart // agent's media attachments (buffered: 1)
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
	Image    *wsImageContent    `json:"image,omitempty"`
}

type wsStreamContent struct {
	ID      string `json:"id"`
	Finish  bool   `json:"finish"`
	Content string `json:"content,omitempty"`
}

// wsImageContent carries a base64-encoded image payload for outbound messages.
type wsImageContent struct {
	Base64 string `json:"base64"`
	MD5    string `json:"md5"`
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
		BaseChannel:  base,
		config:       cfg,
		tasks:        make(map[string]*wsTask),
		tasksByMsgID: make(map[string]*wsTask),
		reqPending:   make(map[string]chan wsEnvelope),
		lastReqIDs:   make(map[string]wsReqIDRecord),
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
	var task *wsTask
	// Prefer exact per-message routing to avoid stale responses going to the
	// wrong (newer) task when two messages arrive in rapid succession.
	if msg.MessageID != "" {
		task = c.tasksByMsgID[msg.MessageID]
	}
	if task == nil {
		task = c.tasks[msg.ChatID]
	}
	c.tasksMu.Unlock()

	if task == nil {
		logger.DebugCF("wecom_aibot", "Send: no active task for chat (may have finished or timed out)",
			map[string]any{"chat_id": msg.ChatID, "message_id": msg.MessageID})
		return nil
	}

	// Non-blocking fast path: when answerCh has space, deliver without racing
	// against task.ctx.Done() (which fires when the task is canceled by a new
	// incoming message, but the response must still be sent).
	select {
	case task.answerCh <- msg.Content:
		return nil
	default:
	}
	// answerCh was full; block with cancellation guards.
	select {
	case task.answerCh <- msg.Content:
	case <-task.ctx.Done():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// SendMedia implements channels.MediaSender.
// if there is an active task for this chat, media is sent to the task's mediaCh and will be delivered after the text reply.
func (c *WeComAIBotWSChannel) SendMedia(ctx context.Context, msg bus.OutboundMediaMessage) error {
	if !c.IsRunning() {
		return channels.ErrNotRunning
	}
	c.tasksMu.Lock()
	task := c.tasks[msg.ChatID]
	c.tasksMu.Unlock()
	logger.InfoCF("wecom_aibot", "SendMedia called",
		map[string]any{"chat_id": msg.ChatID, "parts": len(msg.Parts), "has_task": task != nil})
	if task != nil {
		select {
		case task.mediaCh <- msg.Parts:
			return nil
		case <-task.ctx.Done():
		case <-ctx.Done():
			return ctx.Err()
		default:
			logger.DebugCF("wecom_aibot", "SendMedia: mediaCh full or task done", map[string]any{"chat_id": msg.ChatID})
		}
	}
	reqID := c.getLastReqID(msg.ChatID)
	if reqID == "" {
		logger.WarnCF("wecom_aibot", "SendMedia: no active req_id for chat, dropping media",
			map[string]any{"chat_id": msg.ChatID})
		return nil
	}
	store := c.GetMediaStore()
	if store == nil {
		return nil
	}
	for _, part := range msg.Parts {
		if part.Type == "image" {
			c.sendWSStandaloneImage(reqID, part, store)
		}
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
	c.dispatchWSAgentTask(reqID, msg, msg.Text.Content, nil)
}

// handleWSImageMessage downloads and stores the inbound image, then dispatches
// it to the agent as a media-tagged message.
func (c *WeComAIBotWSChannel) handleWSImageMessage(reqID string, msg WeComAIBotWSMessage) {
	if msg.Image == nil {
		logger.WarnC("wecom_aibot", "Image message missing image field")
		c.wsSendStreamFinish(reqID, wsGenerateID(), "Image message could not be processed.")
		return
	}

	chatID := msg.ChatID
	if chatID == "" {
		chatID = msg.From.UserID
	}

	ctx, cancel := context.WithTimeout(c.ctx, wsImageDownloadTimeout)
	defer cancel()

	var mediaRefs []string
	ref, err := c.storeWSImage(ctx, chatID, msg.MsgID, msg.Image.URL, msg.Image.AESKey)
	if err != nil {
		logger.WarnCF("wecom_aibot", "Failed to download/store WS image",
			map[string]any{"error": err, "url": msg.Image.URL})
	} else {
		mediaRefs = append(mediaRefs, ref)
	}

	c.dispatchWSAgentTask(reqID, msg, "[image]", mediaRefs)
}

// handleWSMixedMessage handles mixed text+image messages.
// All text parts are collected into the content string; all image parts are
// downloaded and stored in MediaStore before dispatching to the agent.
func (c *WeComAIBotWSChannel) handleWSMixedMessage(reqID string, msg WeComAIBotWSMessage) {
	if msg.Mixed == nil {
		logger.WarnC("wecom_aibot", "Mixed message has no content")
		c.wsSendStreamFinish(reqID, wsGenerateID(), "Mixed message type is not yet fully supported.")
		return
	}

	chatID := msg.ChatID
	if chatID == "" {
		chatID = msg.From.UserID
	}

	ctx, cancel := context.WithTimeout(c.ctx, wsImageDownloadTimeout)
	defer cancel()

	var textParts []string
	var mediaRefs []string
	for _, item := range msg.Mixed.MsgItem {
		switch item.MsgType {
		case "text":
			if item.Text != nil && item.Text.Content != "" {
				textParts = append(textParts, item.Text.Content)
			}
		case "image":
			if item.Image != nil {
				ref, err := c.storeWSImage(ctx, chatID,
					msg.MsgID+"-"+wsGenerateID(), item.Image.URL, item.Image.AESKey)
				if err != nil {
					logger.WarnCF("wecom_aibot", "Failed to download/store mixed image",
						map[string]any{"error": err})
				} else {
					mediaRefs = append(mediaRefs, ref)
				}
			}
		}
	}

	if len(textParts) == 0 && len(mediaRefs) == 0 {
		logger.WarnC("wecom_aibot", "Mixed message has no usable content")
		c.wsSendStreamFinish(reqID, wsGenerateID(), "Mixed message type is not yet fully supported.")
		return
	}

	content := strings.Join(textParts, "\n")
	if content == "" {
		content = "[images]"
	}
	c.dispatchWSAgentTask(reqID, msg, content, mediaRefs)
}

// dispatchWSAgentTask registers a new agent task, sends the opening stream frame,
// and starts a goroutine that runs the agent and streams the reply back.
// content is the text forwarded to the agent; mediaRefs are optional media
// store references attached to the inbound message.
func (c *WeComAIBotWSChannel) dispatchWSAgentTask(
	reqID string,
	msg WeComAIBotWSMessage,
	content string,
	mediaRefs []string,
) {
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
		mediaCh:     make(chan []bus.MediaPart, 1),
		ctx:         taskCtx,
		cancel:      taskCancel,
	}

	c.tasksMu.Lock()
	// Cancel any previous task for this chat (user sent a new message mid-reply).
	if prev, ok := c.tasks[chatID]; ok {
		prev.cancel()
	}
	c.tasks[chatID] = task
	// Also register by inbound message ID for precise response routing.
	if msg.MsgID != "" {
		c.tasksByMsgID[msg.MsgID] = task
	}
	c.tasksMu.Unlock()

	// Record this reqID so SendMedia can route images even after the stream closes.
	c.recordLastReqID(chatID, reqID)

	// Send an empty stream opening frame (finish=false) immediately.
	c.wsSendStreamChunk(reqID, streamID, false, "")

	go func() {
		defer func() {
			taskCancel()
			c.tasksMu.Lock()
			if c.tasks[chatID] == task {
				delete(c.tasks, chatID)
			}
			if msg.MsgID != "" && c.tasksByMsgID[msg.MsgID] == task {
				delete(c.tasksByMsgID, msg.MsgID)
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
			"msg_type":  msg.MsgType,
			"msgid":     msg.MsgID,
			"aibotid":   msg.AIBotID,
			"stream_id": streamID,
		}
		c.HandleMessage(taskCtx, peer, msg.MsgID, userID, chatID,
			content, mediaRefs, metadata, sender)

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
				// 1. send final frame with finish=true; any media will come in subsequent frames (if at all)
				c.wsSendStreamFinish(reqID, streamID, answer)
				// 2. wait briefly for any media to arrive via SendMedia → mediaCh,
				// and send each image in its own frame (WeCom does not support multiple images in one frame,
				// and the agent may have sent them at different times)
				var mediaParts []bus.MediaPart
				select {
				case mediaParts = <-task.mediaCh:
					logger.InfoCF("wecom_aibot", "Sending queued media images",
						map[string]any{"chat_id": chatID, "count": len(mediaParts)})
				case <-time.After(wsMediaWaitTimeout):
				}
				if store := c.GetMediaStore(); store != nil {
					for _, part := range mediaParts {
						if part.Type == "image" {
							c.sendWSStandaloneImage(reqID, part, store)
						}
					}
				}
				return
			case <-ticker.C:
				hint := waitHints[tickCount%len(waitHints)]
				tickCount++
				logger.DebugCF("wecom_aibot", "Sending stream progress hint",
					map[string]any{"chat_id": chatID, "tick": tickCount})
				c.wsSendStreamChunk(reqID, streamID, false, hint)
			case <-deadlineTimer.C:
				logger.WarnCF("wecom_aibot", "Stream deadline reached without agent reply",
					map[string]any{"chat_id": chatID, "stream_id": streamID})
				c.wsSendStreamFinish(reqID, streamID,
					"⏳ Processing is taking longer than expected. Please resend your message to try again.")
				return
			case <-taskCtx.Done():
				// Give a short grace period so that a response queued in the bus
				// just before cancellation can still be delivered.  This closes a
				// race where a rapid second message cancels this task after the
				// agent already published but before Send() wrote to answerCh.
				select {
				case answer := <-task.answerCh:
					c.wsSendStreamFinish(reqID, streamID, answer)
				case <-time.After(100 * time.Millisecond):
				}
				return
			}
		}
	}()
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

// wsSendStreamFinish sends the final aibot_respond_msg frame (finish=true, no images).
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

// ---- req_id tracking (for SendMedia after stream close) ----

// recordLastReqID stores the req_id for chatID with a TTL slightly beyond the
// stream max duration so that SendMedia can find it after finish=true is sent.
func (c *WeComAIBotWSChannel) recordLastReqID(chatID, reqID string) {
	c.lastReqIDsMu.Lock()
	c.lastReqIDs[chatID] = wsReqIDRecord{
		ReqID:     reqID,
		ExpiresAt: time.Now().Add(wsStreamMaxDuration + 2*time.Minute),
	}
	c.lastReqIDsMu.Unlock()
}

// getLastReqID returns the most recent req_id for chatID, or "" if none/expired.
func (c *WeComAIBotWSChannel) getLastReqID(chatID string) string {
	c.lastReqIDsMu.Lock()
	r, ok := c.lastReqIDs[chatID]
	c.lastReqIDsMu.Unlock()
	if !ok || time.Now().After(r.ExpiresAt) {
		return ""
	}
	return r.ReqID
}

// ---- Image send helpers ----

// sendWSStandaloneImage reads a media part from store and sends it as a
// standalone aibot_respond_msg with msgtype "image".
func (c *WeComAIBotWSChannel) sendWSStandaloneImage(reqID string, part bus.MediaPart, store media.MediaStore) {
	localPath, err := store.Resolve(part.Ref)
	if err != nil {
		logger.ErrorCF("wecom_aibot", "SendMedia: resolve failed", map[string]any{"ref": part.Ref, "error": err})
		return
	}
	data, err := os.ReadFile(localPath)
	if err != nil {
		logger.ErrorCF("wecom_aibot", "SendMedia: read file failed", map[string]any{"path": localPath, "error": err})
		return
	}
	hash := md5.Sum(data)
	logger.InfoCF("wecom_aibot", "Sending standalone image",
		map[string]any{"req_id": reqID, "path": localPath, "bytes": len(data)})
	c.writeWS(wsCommand{
		Cmd:     "aibot_respond_msg",
		Headers: wsHeaders{ReqID: reqID},
		Body: wsRespondMsgBody{
			MsgType: "image",
			Image: &wsImageContent{
				Base64: base64.StdEncoding.EncodeToString(data),
				MD5:    fmt.Sprintf("%x", hash),
			},
		},
	})
}

// ---- Inbound image download helpers ----

// downloadWSImage fetches and optionally decrypts a WeCom WS image resource.
// aesKey is the per-resource AES key provided in the callback (may be empty).
func (c *WeComAIBotWSChannel) downloadWSImage(ctx context.Context, imageURL, aesKey string) ([]byte, error) {
	const maxSize = 20 << 20 // 20 MB
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	client := &http.Client{Timeout: wsImageDownloadTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSize+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(data) > maxSize {
		return nil, fmt.Errorf("image too large (> %d MB)", maxSize>>20)
	}
	if aesKey == "" {
		return data, nil
	}
	// WeCom per-image AES key: try standard base64 first, then WeCom 43-char format.
	key, decErr := base64.StdEncoding.DecodeString(aesKey)
	if decErr != nil || len(key) != 32 {
		key, decErr = decodeWeComAESKey(aesKey)
		if decErr != nil {
			return nil, fmt.Errorf("decode image AES key: %w", decErr)
		}
	}
	decrypted, err := decryptAESCBC(key, data)
	if err != nil {
		return nil, fmt.Errorf("decrypt image: %w", err)
	}
	return decrypted, nil
}

// storeWSImage downloads, optionally decrypts, and stores an inbound image.
func (c *WeComAIBotWSChannel) storeWSImage(
	ctx context.Context,
	chatID, msgID, imageURL, aesKey string,
) (string, error) {
	store := c.GetMediaStore()
	if store == nil {
		return "", fmt.Errorf("no media store available")
	}
	data, err := c.downloadWSImage(ctx, imageURL, aesKey)
	if err != nil {
		return "", err
	}
	mediaDir := filepath.Join(os.TempDir(), "picoclaw_media")
	if mkErr := os.MkdirAll(mediaDir, 0o700); mkErr != nil {
		return "", fmt.Errorf("mkdir: %w", mkErr)
	}
	filename := msgID + ".jpg"
	localPath := filepath.Join(mediaDir, utils.SanitizeFilename(filename))
	if writeErr := os.WriteFile(localPath, data, 0o600); writeErr != nil {
		return "", fmt.Errorf("write: %w", writeErr)
	}
	scope := channels.BuildMediaScope("wecom_aibot", chatID, msgID)
	ref, err := store.Store(localPath, media.MediaMeta{
		Filename: filename,
		Source:   "wecom_aibot",
	}, scope)
	if err != nil {
		os.Remove(localPath)
		return "", fmt.Errorf("store: %w", err)
	}
	return ref, nil
}
