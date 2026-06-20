package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/uwin/cnc-proxy/internal/jog"
	"github.com/uwin/cnc-proxy/internal/machine"
)

func (s *Server) getJogCapabilities(w http.ResponseWriter, r *http.Request) {
	if s.jog == nil {
		writeJSON(w, http.StatusOK, jog.Capabilities{
			Enabled:      false,
			Axes:         []string{"x", "y", "z"},
			Availability: jog.Availability{Available: false, Reason: jog.CodeDisabled},
		})
		return
	}
	writeJSON(w, http.StatusOK, s.jog.Capabilities())
}

type jogClientMessage struct {
	Type     string             `json:"type"`
	Seq      int64              `json:"seq"`
	Deadman  bool               `json:"deadman"`
	Axes     map[string]float64 `json:"axes"`
	Slow     bool               `json:"slow"`
	Action   string             `json:"action"`
	Axis     string             `json:"axis"`
	Distance float64            `json:"distance"`
	Target   map[string]float64 `json:"target"`
	Feed     float64            `json:"feed_mm_min"`
}

func (s *Server) jogWS(w http.ResponseWriter, r *http.Request) {
	if s.jog == nil {
		writeErr(w, http.StatusServiceUnavailable, jog.CodeDisabled)
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	c.SetReadLimit(4096)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	sess, err := s.jog.Start(ctx)
	if err != nil {
		writeWSEvent(ctx, c, jog.Event{Type: "error", Code: jog.CodeBusy, Message: err.Error()})
		c.Close(websocket.StatusPolicyViolation, err.Error())
		return
	}
	defer sess.Close()

	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		for ev := range sess.Events() {
			if err := writeWSEvent(ctx, c, ev); err != nil {
				cancel()
				return
			}
		}
	}()

	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			cancel()
			<-writeDone
			return
		}
		var msg jogClientMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			sess.ReportError(0, jog.CodeBadInput, "invalid JSON: "+err.Error())
			continue
		}
		switch msg.Type {
		case "arm":
			sess.Arm(msg.Seq)
		case "disarm":
			sess.Disarm(msg.Seq)
		case "control":
			sess.Control(msg.Seq, msg.Action)
		case "input":
			axes, err := parseJogAxes(msg.Axes)
			if err != nil {
				sess.ReportError(msg.Seq, jog.CodeBadInput, err.Error())
				continue
			}
			sess.SetInput(jog.Input{Seq: msg.Seq, Axes: axes, Deadman: msg.Deadman, Slow: msg.Slow})
		case "step":
			sess.Step(msg.Seq, msg.Axis, msg.Distance)
		case "target":
			target, err := parseJogTarget(msg.Target)
			if err != nil {
				sess.ReportError(msg.Seq, jog.CodeBadInput, err.Error())
				continue
			}
			sess.Target(msg.Seq, target, msg.Feed)
		default:
			sess.ReportError(msg.Seq, jog.CodeBadInput, "type must be one of: arm, input, target, step, control, disarm")
		}
	}
}

func writeWSEvent(ctx context.Context, c *websocket.Conn, ev jog.Event) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return c.Write(wctx, websocket.MessageText, b)
}

func parseJogAxes(in map[string]float64) (jog.Axes, error) {
	var out jog.Axes
	for k, v := range in {
		if v < -1 || v > 1 {
			return out, fmt.Errorf("axis %q must be between -1 and 1", k)
		}
		switch k {
		case "x":
			out.X = v
		case "y":
			out.Y = v
		case "z":
			out.Z = v
		default:
			return out, fmt.Errorf("unsupported axis %q", k)
		}
	}
	return out, nil
}

func parseJogTarget(in map[string]float64) (machine.AxisValues, error) {
	x, okX := in["x"]
	y, okY := in["y"]
	if !okX || !okY {
		return nil, fmt.Errorf("target requires x and y")
	}
	for k, v := range in {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("target %q must be finite", k)
		}
		if k != "x" && k != "y" {
			return nil, fmt.Errorf("unsupported target axis %q", k)
		}
	}
	return machine.AxisValues{"x": x, "y": y}, nil
}
