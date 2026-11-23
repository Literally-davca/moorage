package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"moorage/internal/host"
	"moorage/internal/notify"
	"moorage/internal/store"
	"moorage/internal/webpush"
)

// ---- event notifications ------------------------------------------------------

// eventChannels returns the enabled channels subscribed to event.
func (s *Server) eventChannels(ctx context.Context, event string) []store.Channel {
	chs, err := s.store.Channels(ctx)
	if err != nil {
		return nil
	}
	var out []store.Channel
	for _, ch := range chs {
		var cfg notify.Config
		if ch.Enabled && json.Unmarshal([]byte(ch.Config), &cfg) == nil && cfg.WantsEvent(event) {
			out = append(out, ch)
		}
	}
	return out
}

// wantsEvent reports whether any channel or browser would hear about event.
func (s *Server) wantsEvent(ctx context.Context, event string) bool {
	if len(s.eventChannels(ctx, event)) > 0 {
		return true
	}
	subs, err := s.store.PushSubscriptions(ctx, 0)
	return err == nil && len(subs) > 0
}

// notifyEvent sends msg to the channels subscribed to event and to Web Push.
func (s *Server) notifyEvent(ctx context.Context, event string, msg notify.Message) {
	for _, ch := range s.eventChannels(ctx, event) {
		var cfg notify.Config
		if json.Unmarshal([]byte(ch.Config), &cfg) != nil {
			continue
		}
		if err := notify.Send(ctx, ch.Kind, cfg, msg); err != nil {
			s.log.Warn("event notification failed", "event", event, "channel", ch.Name, "err", err)
		}
	}
	s.push.Broadcast(ctx, event, msg)
}

func (s *Server) stackLink(hostID, stack string) string {
	base := strings.TrimRight(s.cfg.PublicURL, "/")
	if base == "" {
		return ""
	}
	return fmt.Sprintf("%s/#/h/%s/stacks/%s", base, hostID, stack)
}

// watchOp follows a stack operation in the background and sends a deploy_failed event
// when it fails. It does nothing when nobody listens for the event.
func (s *Server) watchOp(h host.Host, hostID, stack, action, opID string) {
	if opID == "" || !s.wantsEvent(context.Background(), notify.EventDeployFailed) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
		defer cancel()
		var opErr string
		if err := h.FollowOp(ctx, opID, func(ev host.OpEvent) error {
			if ev.Done {
				opErr = ev.Error
			}
			return nil
		}); err != nil || opErr == "" {
			return
		}
		hostName := hostID
		if s.reg != nil {
			if _, meta, err := s.reg.Get(ctx, hostID); err == nil && meta.Name != "" {
				hostName = meta.Name
			}
		}
		s.notifyEvent(ctx, notify.EventDeployFailed, notify.Message{
			Status: "down",
			Title:  fmt.Sprintf("%s failed: %s", capitalize(action), stack),
			Text:   fmt.Sprintf("%s on %s: %s", stack, hostName, opErr),
			URL:    s.stackLink(hostID, stack),
		})
	}()
}

// ---- Web Push subscriptions --------------------------------------------------

func (s *Server) pushKey(w http.ResponseWriter, r *http.Request) {
	key, err := s.push.PublicKey()
	if err != nil {
		s.engErr(w, err)
		return
	}
	subs, _ := s.store.PushSubscriptions(r.Context(), currentUser(r).ID)
	endpoints := make([]string, 0, len(subs))
	for _, sub := range subs {
		endpoints = append(endpoints, sub.Endpoint)
	}
	writeJSON(w, http.StatusOK, map[string]any{"publicKey": key, "endpoints": endpoints})
}

func (s *Server) pushSubscribe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256DH string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := webpush.ValidateSubscription(req.Endpoint, req.Keys.P256DH, req.Keys.Auth); err != nil {
		writeErr(w, http.StatusBadRequest, capitalize(err.Error())+".")
		return
	}
	if err := s.store.SavePushSubscription(r.Context(), store.PushSubscription{
		UserID: currentUser(r).ID, Endpoint: req.Endpoint, P256DH: req.Keys.P256DH, Auth: req.Keys.Auth,
	}); err != nil {
		s.engErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) pushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Endpoint string `json:"endpoint"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if err := s.store.DeletePushSubscription(r.Context(), currentUser(r).ID, req.Endpoint); err != nil {
		s.engErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) pushTest(w http.ResponseWriter, r *http.Request) {
	n, err := s.push.SendTest(r.Context(), currentUser(r).ID)
	if err != nil {
		if errors.Is(err, webpush.ErrGone) {
			writeErr(w, http.StatusGone, "This device's subscription expired. Turn push notifications on again.")
			return
		}
		writeErr(w, http.StatusBadGateway, "Push failed: "+err.Error())
		return
	}
	if n == 0 {
		writeErr(w, http.StatusNotFound, "No device of yours is subscribed.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"sent": n})
}
