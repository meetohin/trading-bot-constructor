package websocket

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tinkoff/invest-api-go-sdk/investgo"
	"go.uber.org/zap"
)

// Hub - центральный хаб для управления WebSocket соединениями
type Hub struct {
	clients    map[*Client]bool
	register   chan *Client
	unregister chan *Client
	broadcast  chan []byte
	mu         sync.RWMutex
	logger     *zap.SugaredLogger
}

// Client - представляет WebSocket клиента
type Client struct {
	hub      *Hub
	conn     *websocket.Conn
	send     chan []byte
	userID   string
	clientID string

	// Подписки
	subscriptions map[string]bool
	mu            sync.RWMutex
}

// Message - структура сообщения WebSocket
type Message struct {
	Type      string      `json:"type"`
	Action    string      `json:"action,omitempty"`
	Data      interface{} `json:"data,omitempty"`
	Error     string      `json:"error,omitempty"`
	Timestamp int64       `json:"timestamp"`
	ClientID  string      `json:"client_id,omitempty"`
}

// Subscription - структура подписки
type Subscription struct {
	Type        string   `json:"type"`
	Instruments []string `json:"instruments,omitempty"`
	AccountIDs  []string `json:"account_ids,omitempty"`
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // В продакшене нужна более строгая проверка
	},
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

const (
	// Таймауты
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 512
)

// NewHub - создание нового хаба
func NewHub(logger *zap.SugaredLogger) *Hub {
	return &Hub{
		clients:    make(map[*Client]bool),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		broadcast:  make(chan []byte),
		logger:     logger,
	}
}

// Run - запуск хаба
func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = true
			h.mu.Unlock()
			h.logger.Infof("Client %s connected", client.clientID)

			// Отправляем приветственное сообщение
			welcomeMsg := Message{
				Type:      "system",
				Action:    "connected",
				Data:      map[string]string{"client_id": client.clientID},
				Timestamp: time.Now().Unix(),
			}
			client.SendMessage(welcomeMsg)

		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
				h.logger.Infof("Client %s disconnected", client.clientID)
			}
			h.mu.Unlock()

		case message := <-h.broadcast:
			h.mu.RLock()
			for client := range h.clients {
				select {
				case client.send <- message:
				default:
					delete(h.clients, client)
					close(client.send)
				}
			}
			h.mu.RUnlock()
		}
	}
}

// Broadcast - отправка сообщения всем клиентам
func (h *Hub) Broadcast(message Message) {
	data, err := json.Marshal(message)
	if err != nil {
		h.logger.Errorf("Failed to marshal broadcast message: %v", err)
		return
	}

	h.broadcast <- data
}

// BroadcastToSubscribers - отправка сообщения подписчикам
func (h *Hub) BroadcastToSubscribers(subscriptionType string, message Message) {
	data, err := json.Marshal(message)
	if err != nil {
		h.logger.Errorf("Failed to marshal message: %v", err)
		return
	}

	h.mu.RLock()
	defer h.mu.RUnlock()

	for client := range h.clients {
		client.mu.RLock()
		if client.subscriptions[subscriptionType] {
			select {
			case client.send <- data:
			default:
				delete(h.clients, client)
				close(client.send)
			}
		}
		client.mu.RUnlock()
	}
}

// WebSocketHandler - обработчик WebSocket соединений
func WebSocketHandler(hub *Hub, tradingServer interface{}) gin.HandlerFunc {
	return func(c *gin.Context) {
		conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			hub.logger.Errorf("Failed to upgrade connection: %v", err)
			return
		}

		clientID := generateClientID()
		userID := c.Query("user_id")
		if userID == "" {
			userID = "anonymous"
		}

		client := &Client{
			hub:           hub,
			conn:          conn,
			send:          make(chan []byte, 256),
			userID:        userID,
			clientID:      clientID,
			subscriptions: make(map[string]bool),
		}

		hub.register <- client

		// Запускаем горутины для чтения и записи
		go client.writePump()
		go client.readPump()
	}
}

// SendMessage - отправка сообщения клиенту
func (c *Client) SendMessage(message Message) {
	message.ClientID = c.clientID
	message.Timestamp = time.Now().Unix()

	data, err := json.Marshal(message)
	if err != nil {
		c.hub.logger.Errorf("Failed to marshal message: %v", err)
		return
	}

	select {
	case c.send <- data:
	default:
		close(c.send)
	}
}

// readPump - чтение сообщений от клиента
func (c *Client) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.conn.Close()
	}()

	c.conn.SetReadLimit(maxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, messageData, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				c.hub.logger.Errorf("WebSocket error: %v", err)
			}
			break
		}

		var message Message
		if err := json.Unmarshal(messageData, &message); err != nil {
			c.hub.logger.Errorf("Failed to unmarshal message: %v", err)
			c.SendMessage(Message{
				Type:  "error",
				Error: "Invalid message format",
			})
			continue
		}

		c.handleMessage(message)
	}
}

// writePump - отправка сообщений клиенту
func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := c.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write(message)

			// Отправляем дополнительные сообщения из очереди
			n := len(c.send)
			for i := 0; i < n; i++ {
				w.Write([]byte("\n"))
				w.Write(<-c.send)
			}

			if err := w.Close(); err != nil {
				return
			}

		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// handleMessage - обработка входящих сообщений
func (c *Client) handleMessage(message Message) {
	switch message.Type {
	case "subscribe":
		c.handleSubscription(message)
	case "unsubscribe":
		c.handleUnsubscription(message)
	case "ping":
		c.SendMessage(Message{
			Type:   "pong",
			Action: "ping_response",
		})
	default:
		c.SendMessage(Message{
			Type:  "error",
			Error: "Unknown message type",
		})
	}
}

// handleSubscription - обработка подписки
func (c *Client) handleSubscription(message Message) {
	var subscription Subscription
	data, _ := json.Marshal(message.Data)
	if err := json.Unmarshal(data, &subscription); err != nil {
		c.SendMessage(Message{
			Type:  "error",
			Error: "Invalid subscription format",
		})
		return
	}

	c.mu.Lock()
	c.subscriptions[subscription.Type] = true
	c.mu.Unlock()

	c.hub.logger.Infof("Client %s subscribed to %s", c.clientID, subscription.Type)

	c.SendMessage(Message{
		Type:   "subscription",
		Action: "subscribed",
		Data:   subscription,
	})
}

// handleUnsubscription - обработка отписки
func (c *Client) handleUnsubscription(message Message) {
	var subscription Subscription
	data, _ := json.Marshal(message.Data)
	if err := json.Unmarshal(data, &subscription); err != nil {
		c.SendMessage(Message{
			Type:  "error",
			Error: "Invalid unsubscription format",
		})
		return
	}

	c.mu.Lock()
	delete(c.subscriptions, subscription.Type)
	c.mu.Unlock()

	c.hub.logger.Infof("Client %s unsubscribed from %s", c.clientID, subscription.Type)

	c.SendMessage(Message{
		Type:   "subscription",
		Action: "unsubscribed",
		Data:   subscription,
	})
}

// StreamManager - менеджер для управления стримами данных
type StreamManager struct {
	hub    *Hub
	client *investgo.Client
	logger *zap.SugaredLogger
	ctx    context.Context
	cancel context.CancelFunc

	marketDataStream *investgo.MarketDataStreamClient
	operationsStream *investgo.OperationsStreamClient
}

// NewStreamManager - создание менеджера стримов
func NewStreamManager(hub *Hub, client *investgo.Client, logger *zap.SugaredLogger) *StreamManager {
	ctx, cancel := context.WithCancel(context.Background())

	return &StreamManager{
		hub:              hub,
		client:           client,
		logger:           logger,
		ctx:              ctx,
		cancel:           cancel,
		marketDataStream: client.NewMarketDataStreamClient(),
		operationsStream: client.NewOperationsStreamClient(),
	}
}

// Start - запуск менеджера стримов
func (sm *StreamManager) Start() error {
	sm.logger.Info("Starting stream manager...")

	// Запускаем стрим маркетдаты
	go sm.startMarketDataStream()

	// Запускаем стрим операций
	go sm.startOperationsStream()

	return nil
}

// Stop - остановка менеджера стримов
func (sm *StreamManager) Stop() {
	sm.logger.Info("Stopping stream manager...")
	sm.cancel()
}

// startMarketDataStream - запуск стрима маркетдаты
func (sm *StreamManager) startMarketDataStream() {
	// Здесь должна быть логика подключения к стриму маркетдаты
	// и отправка данных через WebSocket
	sm.logger.Info("Market data stream started")
}

// startOperationsStream - запуск стрима операций
func (sm *StreamManager) startOperationsStream() {
	// Здесь должна быть логика подключения к стриму операций
	// и отправка данных через WebSocket
	sm.logger.Info("Operations stream started")
}

// generateClientID - генерация ID клиента
func generateClientID() string {
	return fmt.Sprintf("client_%d", time.Now().UnixNano())
}
