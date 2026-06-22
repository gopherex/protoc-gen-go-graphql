package graphqlrt

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/graphql-go/graphql"
)

// graphql-transport-ws message types (Apollo / urql compatible).
const (
	wsConnectionInit = "connection_init"
	wsConnectionAck  = "connection_ack"
	wsPing           = "ping"
	wsPong           = "pong"
	wsSubscribe      = "subscribe"
	wsNext           = "next"
	wsError          = "error"
	wsComplete       = "complete"
)

type wsMessage struct {
	ID      string          `json:"id,omitempty"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type wsSubscribePayload struct {
	Query         string                 `json:"query"`
	OperationName string                 `json:"operationName,omitempty"`
	Variables     map[string]interface{} `json:"variables,omitempty"`
}

// SubscriptionHandler serves GraphQL subscriptions over WebSocket using the
// graphql-transport-ws protocol, executing against schema via graphql.Subscribe.
// baseContext, if non-nil, derives the per-connection context from the upgrade
// request (e.g. to bridge an Authorization header into the context the generated
// resolvers' Authorize hook reads); pass nil for r.Context().
func SubscriptionHandler(schema *graphql.Schema, baseContext func(*http.Request) context.Context) http.Handler {
	upgrader := websocket.Upgrader{
		Subprotocols:    []string{"graphql-transport-ws"},
		CheckOrigin:     func(*http.Request) bool { return true },
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		base := r.Context()
		if baseContext != nil {
			base = baseContext(r)
		}
		(&wsConn{schema: schema, conn: conn, base: base, subs: map[string]context.CancelFunc{}}).serve()
	})
}

type wsConn struct {
	schema *graphql.Schema
	conn   *websocket.Conn
	base   context.Context
	mu     sync.Mutex // serializes writes
	subsMu sync.Mutex
	subs   map[string]context.CancelFunc
	acked  bool
}

func (c *wsConn) write(m wsMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.WriteJSON(m)
}

func (c *wsConn) serve() {
	defer c.closeAll()
	for {
		var m wsMessage
		if err := c.conn.ReadJSON(&m); err != nil {
			return
		}
		switch m.Type {
		case wsConnectionInit:
			if !c.acked {
				c.acked = true
				_ = c.write(wsMessage{Type: wsConnectionAck})
			}
		case wsPing:
			_ = c.write(wsMessage{Type: wsPong})
		case wsPong:
			// no-op
		case wsSubscribe:
			c.startSubscription(m)
		case wsComplete:
			c.stopSubscription(m.ID)
		default:
			// ignore unknown types
		}
	}
}

func (c *wsConn) startSubscription(m wsMessage) {
	if m.ID == "" {
		return
	}
	var p wsSubscribePayload
	if err := json.Unmarshal(m.Payload, &p); err != nil {
		_ = c.write(wsMessage{ID: m.ID, Type: wsError, Payload: jsonRaw(graphqlErrorsJSON(err.Error()))})
		return
	}
	ctx, cancel := context.WithCancel(c.base)
	c.subsMu.Lock()
	if old, ok := c.subs[m.ID]; ok {
		old() // subscriber id reuse: cancel the previous one
	}
	c.subs[m.ID] = cancel
	c.subsMu.Unlock()

	results := graphql.Subscribe(graphql.Params{
		Schema:         *c.schema,
		RequestString:  p.Query,
		OperationName:  p.OperationName,
		VariableValues: p.Variables,
		Context:        ctx,
	})

	go func() {
		defer c.stopSubscription(m.ID)
		for {
			select {
			case <-ctx.Done():
				return
			case res, ok := <-results:
				if !ok {
					_ = c.write(wsMessage{ID: m.ID, Type: wsComplete})
					return
				}
				payload, _ := json.Marshal(res)
				if len(res.Errors) > 0 && res.Data == nil {
					_ = c.write(wsMessage{ID: m.ID, Type: wsError, Payload: payload})
					return
				}
				if err := c.write(wsMessage{ID: m.ID, Type: wsNext, Payload: payload}); err != nil {
					return
				}
			}
		}
	}()
}

func (c *wsConn) stopSubscription(id string) {
	c.subsMu.Lock()
	if cancel, ok := c.subs[id]; ok {
		cancel()
		delete(c.subs, id)
	}
	c.subsMu.Unlock()
}

func (c *wsConn) closeAll() {
	c.subsMu.Lock()
	for id, cancel := range c.subs {
		cancel()
		delete(c.subs, id)
	}
	c.subsMu.Unlock()
	_ = c.conn.Close()
}

func jsonRaw(b []byte) json.RawMessage { return json.RawMessage(b) }

func graphqlErrorsJSON(msg string) []byte {
	b, _ := json.Marshal([]map[string]string{{"message": msg}})
	return b
}
