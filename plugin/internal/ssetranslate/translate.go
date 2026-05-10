package ssetranslate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Writer ingests stream-json bytes and emits SSE bytes on the inner
// writer. Single-flight: one goroutine only.
type Writer struct {
	inner io.Writer

	// Buffered partial line — Write may receive arbitrary chunk
	// boundaries, so we accumulate until we see '\n'.
	buf bytes.Buffer

	state state
	envel envelope // captured between message_start and message_stop

	// firstWriteErr latches the first inner.Write error; subsequent
	// Writes return it without further work.
	firstWriteErr error
	closed        bool
}

type state int

const (
	stateIdle state = iota
	stateInMessage
	stateDone
)

// envelope is the per-message state we accumulate between message_start
// and message_stop. content blocks open as they appear; when the next
// block index appears (or message ends), the previous one closes.
type envelope struct {
	id           string
	openBlockIdx int  // index of currently-open content block
	hasOpenBlock bool // is openBlockIdx valid?
	nextBlockIdx int  // next index to assign for new blocks
	stopReason   *string
	stopSequence *string
	finalUsage   *usage
}

// NewWriter returns a Writer wrapping inner. Callers should wrap once
// per logical /v1/messages turn.
func NewWriter(inner io.Writer) *Writer {
	return &Writer{inner: inner, state: stateIdle}
}

// Write ingests stream-json bytes. Returns len(p), nil on success even
// when zero SSE events are emitted. A first inner.Write error latches;
// subsequent Writes return that error.
func (w *Writer) Write(p []byte) (int, error) {
	if w.firstWriteErr != nil {
		return 0, w.firstWriteErr
	}
	if w.closed {
		return 0, errors.New("ssetranslate: write after close")
	}

	w.buf.Write(p)
	for {
		line, err := w.buf.ReadBytes('\n')
		if err != nil {
			// No newline yet — put the partial back.
			if len(line) > 0 {
				w.buf.Write(line)
			}
			break
		}
		if errInner := w.handleLine(line); errInner != nil {
			w.firstWriteErr = errInner
			return 0, errInner
		}
	}
	return len(p), nil
}

// Close finalizes any open envelope. Idempotent.
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if w.firstWriteErr != nil {
		return nil
	}
	if w.state == stateInMessage {
		if err := w.finishMessage(synthStopReason("end_turn"), nil, w.envel.finalUsage); err != nil {
			w.firstWriteErr = err
			return err
		}
	}
	return nil
}

func synthStopReason(s string) *string { return &s }

// handleLine processes one newline-terminated stream-json line.
func (w *Writer) handleLine(line []byte) error {
	body := bytes.TrimRight(line, "\r\n")
	if len(body) == 0 || body[0] != '{' {
		return nil // tolerate malformed/empty lines
	}
	var head struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		return nil
	}
	switch head.Type {
	case "assistant":
		return w.handleAssistant(body)
	case "result":
		return w.handleResult(body)
	default:
		// system, user, tool_result, anything else: drop.
		return nil
	}
}

// --- assistant event handling ---

type assistantEnvelope struct {
	Type    string `json:"type"`
	Message struct {
		ID           string            `json:"id"`
		Type         string            `json:"type"`
		Role         string            `json:"role"`
		Model        string            `json:"model"`
		Content      []json.RawMessage `json:"content"`
		StopReason   *string           `json:"stop_reason"`
		StopSequence *string           `json:"stop_sequence"`
		Usage        *usage            `json:"usage"`
	} `json:"message"`
}

type usage struct {
	InputTokens  uint64 `json:"input_tokens"`
	OutputTokens uint64 `json:"output_tokens"`
}

func (w *Writer) handleAssistant(body []byte) error {
	var ev assistantEnvelope
	if err := json.Unmarshal(body, &ev); err != nil {
		return nil
	}
	if w.state == stateDone {
		// New assistant after we've already closed — defensive ignore.
		return nil
	}
	if w.state == stateIdle {
		if err := w.emitMessageStart(ev); err != nil {
			return err
		}
		w.state = stateInMessage
		w.envel.id = ev.Message.ID
	}
	// Cache rolling stop_reason/stop_sequence/usage from the latest cycle.
	w.envel.stopReason = ev.Message.StopReason
	w.envel.stopSequence = ev.Message.StopSequence
	if ev.Message.Usage != nil {
		w.envel.finalUsage = ev.Message.Usage
	}
	for _, blk := range ev.Message.Content {
		if err := w.handleContentBlock(blk); err != nil {
			return err
		}
	}
	return nil
}

// handleContentBlock handles one block from an assistant event. Each
// block from stream-json is fully formed; we either open+populate+close
// it or extend an already-open block of the same type/index.
func (w *Writer) handleContentBlock(blk json.RawMessage) error {
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(blk, &head); err != nil {
		return nil
	}
	switch head.Type {
	case "text":
		return w.handleTextBlock(blk)
	case "tool_use":
		return w.handleToolUseBlock(blk)
	default:
		// Unknown block types (thinking, image, etc.) — ship as
		// content_block_start with the raw block, then immediately
		// content_block_stop, no delta. Future work may expand this.
		return w.shipOpaqueBlock(blk)
	}
}

func (w *Writer) handleTextBlock(blk json.RawMessage) error {
	var b struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(blk, &b); err != nil {
		return nil
	}
	if !w.envel.hasOpenBlock {
		idx := w.envel.nextBlockIdx
		if err := w.emitContentBlockStart(idx, json.RawMessage(`{"type":"text","text":""}`)); err != nil {
			return err
		}
		w.envel.openBlockIdx = idx
		w.envel.hasOpenBlock = true
		w.envel.nextBlockIdx++
	}
	if b.Text != "" {
		if err := w.emitTextDelta(w.envel.openBlockIdx, b.Text); err != nil {
			return err
		}
	}
	return nil
}

func (w *Writer) handleToolUseBlock(blk json.RawMessage) error {
	if w.envel.hasOpenBlock {
		if err := w.emitContentBlockStop(w.envel.openBlockIdx); err != nil {
			return err
		}
		w.envel.hasOpenBlock = false
	}
	idx := w.envel.nextBlockIdx
	if err := w.emitContentBlockStart(idx, blk); err != nil {
		return err
	}
	if err := w.emitContentBlockStop(idx); err != nil {
		return err
	}
	w.envel.nextBlockIdx++
	return nil
}

func (w *Writer) shipOpaqueBlock(blk json.RawMessage) error {
	if w.envel.hasOpenBlock {
		if err := w.emitContentBlockStop(w.envel.openBlockIdx); err != nil {
			return err
		}
		w.envel.hasOpenBlock = false
	}
	idx := w.envel.nextBlockIdx
	if err := w.emitContentBlockStart(idx, blk); err != nil {
		return err
	}
	if err := w.emitContentBlockStop(idx); err != nil {
		return err
	}
	w.envel.nextBlockIdx++
	return nil
}

// --- result event handling ---

type resultEnvelope struct {
	Type    string `json:"type"`
	IsError bool   `json:"is_error"`
	Usage   *usage `json:"usage"`
}

func (w *Writer) handleResult(body []byte) error {
	var ev resultEnvelope
	if err := json.Unmarshal(body, &ev); err != nil {
		return nil
	}
	if w.state != stateInMessage {
		return nil // result without prior assistant — drop
	}
	stopReason := w.envel.stopReason
	if stopReason == nil {
		var def string
		if ev.IsError {
			def = "error"
		} else {
			def = "end_turn"
		}
		stopReason = &def
	}
	useUsage := ev.Usage
	if useUsage == nil {
		useUsage = w.envel.finalUsage
	}
	return w.finishMessage(stopReason, w.envel.stopSequence, useUsage)
}

// finishMessage closes any open block and emits message_delta + message_stop.
func (w *Writer) finishMessage(stopReason, stopSequence *string, u *usage) error {
	if w.envel.hasOpenBlock {
		if err := w.emitContentBlockStop(w.envel.openBlockIdx); err != nil {
			return err
		}
		w.envel.hasOpenBlock = false
	}
	if err := w.emitMessageDelta(stopReason, stopSequence, u); err != nil {
		return err
	}
	if err := w.emitMessageStop(); err != nil {
		return err
	}
	w.state = stateDone
	return nil
}

// --- SSE wire types (field order preserved via struct tags) ---

type sseMessageStart struct {
	Type    string         `json:"type"`
	Message sseMessageBody `json:"message"`
}

type sseMessageBody struct {
	ID           string  `json:"id"`
	Type         string  `json:"type"`
	Role         string  `json:"role"`
	Model        string  `json:"model"`
	Content      []any   `json:"content"`
	StopReason   *string `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
	Usage        *usage  `json:"usage"`
}

type sseContentBlockStart struct {
	Type         string          `json:"type"`
	Index        int             `json:"index"`
	ContentBlock json.RawMessage `json:"content_block"`
}

type sseContentBlockDelta struct {
	Type  string       `json:"type"`
	Index int          `json:"index"`
	Delta sseTextDelta `json:"delta"`
}

type sseTextDelta struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type sseContentBlockStop struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

type sseMessageDeltaPayload struct {
	Type  string           `json:"type"`
	Delta sseMessageDeltaD `json:"delta"`
	Usage *usage           `json:"usage,omitempty"`
}

type sseMessageDeltaD struct {
	StopReason   *string `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
}

type sseMessageStop struct {
	Type string `json:"type"`
}

// --- emit helpers ---

func (w *Writer) emit(event string, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("ssetranslate: marshal %s: %w", event, err)
	}
	if _, err := fmt.Fprintf(w.inner, "event: %s\ndata: %s\n\n", event, payload); err != nil {
		return fmt.Errorf("ssetranslate: write %s: %w", event, err)
	}
	return nil
}

func (w *Writer) emitMessageStart(ev assistantEnvelope) error {
	msg := sseMessageStart{
		Type: "message_start",
		Message: sseMessageBody{
			ID:           ev.Message.ID,
			Type:         ev.Message.Type,
			Role:         ev.Message.Role,
			Model:        ev.Message.Model,
			Content:      []any{},
			StopReason:   ev.Message.StopReason,
			StopSequence: ev.Message.StopSequence,
			Usage:        ev.Message.Usage,
		},
	}
	return w.emit("message_start", msg)
}

func (w *Writer) emitContentBlockStart(idx int, block json.RawMessage) error {
	return w.emit("content_block_start", sseContentBlockStart{
		Type:         "content_block_start",
		Index:        idx,
		ContentBlock: block,
	})
}

func (w *Writer) emitTextDelta(idx int, text string) error {
	return w.emit("content_block_delta", sseContentBlockDelta{
		Type:  "content_block_delta",
		Index: idx,
		Delta: sseTextDelta{Type: "text_delta", Text: text},
	})
}

func (w *Writer) emitContentBlockStop(idx int) error {
	return w.emit("content_block_stop", sseContentBlockStop{
		Type:  "content_block_stop",
		Index: idx,
	})
}

func (w *Writer) emitMessageDelta(stopReason, stopSequence *string, u *usage) error {
	return w.emit("message_delta", sseMessageDeltaPayload{
		Type:  "message_delta",
		Delta: sseMessageDeltaD{StopReason: stopReason, StopSequence: stopSequence},
		Usage: u,
	})
}

func (w *Writer) emitMessageStop() error {
	return w.emit("message_stop", sseMessageStop{Type: "message_stop"})
}

// Compile-time interface check.
var _ io.WriteCloser = (*Writer)(nil)
