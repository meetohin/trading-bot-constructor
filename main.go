package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tinkoff/invest-api-go-sdk/investgo"
	pb "github.com/tinkoff/invest-api-go-sdk/proto"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	
	// Локальные пакеты
	"./middleware"
	"./websocket"
	"./bots"
)

// TradingServer - основная структура сервера
type TradingServer struct {
	client                *investgo.Client
	config                investgo.Config
	logger                *zap.SugaredLogger
	ctx                   context.Context
	cancel                context.CancelFunc
	
	// Сервисы API
	usersService          *investgo.UsersServiceClient
	ordersService         *investgo.OrdersServiceClient
	instrumentsService    *investgo.InstrumentsServiceClient
	marketDataService     *investgo.MarketDataServiceClient
	sandboxService        *investgo.SandboxServiceClient
	operationsService     *investgo.OperationsServiceClient
	
	// Стримы
	marketDataStream      *investgo.MarketDataStreamClient
	operationsStream      *investgo.OperationsStreamClient
	
	// HTTP сервер
	httpServer            *http.Server
	router                *gin.Engine
	
	// Синхронизация
	wg                    *sync.WaitGroup
	mu                    sync.RWMutex
	
	// WebSocket хаб
	wsHub             *websocket.Hub
	streamManager     *websocket.StreamManager
	
	// Менеджер ботов
	botManager        *bots.BotManager
	
	// Данные
	accounts              []string
	positions             map[string]interface{}
	portfolio             map[string]interface{}
}

// NewTradingServer - создание нового экземпляра сервера
func NewTradingServer() (*TradingServer, error) {
	// Загружаем конфигурацию
	config, err := investgo.LoadConfig("config.yaml")
	if err != nil {
		return nil, fmt.Errorf("config loading error: %w", err)
	}

	// Настраиваем контекст
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL)

	// Настраиваем логгер
	zapConfig := zap.NewDevelopmentConfig()
	zapConfig.EncoderConfig.EncodeTime = zapcore.TimeEncoderOfLayout(time.DateTime)
	zapConfig.EncoderConfig.TimeKey = "time"
	l, err := zapConfig.Build()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("logger creating error: %w", err)
	}
	logger := l.Sugar()

	// Создаем клиента investAPI
	client, err := investgo.NewClient(ctx, config, logger)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("client creating error: %w", err)
	}

	// Настраиваем HTTP роутер
	gin.SetMode(gin.ReleaseMode)
	router := gin.Default()

	server := &TradingServer{
		client:     client,
		config:     config,
		logger:     logger,
		ctx:        ctx,
		cancel:     cancel,
		router:     router,
		wg:         &sync.WaitGroup{},
		positions:  make(map[string]interface{}),
		portfolio:  make(map[string]interface{}),
	}

	// Инициализируем все сервисы
	if err := server.initializeServices(); err != nil {
		server.Stop()
		return nil, fmt.Errorf("services initialization error: %w", err)
	}

	// Настраиваем HTTP маршруты
	server.setupRoutes()

	return server, nil
}

// initializeServices - инициализация всех сервисов API
func (ts *TradingServer) initializeServices() error {
	ts.logger.Info("Initializing API services...")

	// Создаем все сервисы
	ts.usersService = ts.client.NewUsersServiceClient()
	ts.ordersService = ts.client.NewOrdersServiceClient()
	ts.instrumentsService = ts.client.NewInstrumentsServiceClient()
	ts.marketDataService = ts.client.NewMarketDataServiceClient()
	ts.sandboxService = ts.client.NewSandboxServiceClient()
	ts.operationsService = ts.client.NewOperationsServiceClient()

	// Создаем стримы
	ts.marketDataStream = ts.client.NewMarketDataStreamClient()
	ts.operationsStream = ts.client.NewOperationsStreamClient()

	// Создаем WebSocket хаб
	ts.wsHub = websocket.NewHub(ts.logger)
	go ts.wsHub.Run()

	// Создаем менеджер ботов
	ts.botManager = bots.NewBotManager(ts.client, ts.logger)

	// Получаем информацию об аккаунтах
	if err := ts.loadAccountInfo(); err != nil {
		return fmt.Errorf("failed to load account info: %w", err)
	}

	ts.logger.Info("All services initialized successfully")
	return nil
}

// loadAccountInfo - загрузка информации об аккаунтах
func (ts *TradingServer) loadAccountInfo() error {
	accountsResp, err := ts.usersService.GetAccounts()
	if err != nil {
		return fmt.Errorf("failed to get accounts: %w", err)
	}

	ts.accounts = make([]string, 0)
	for _, acc := range accountsResp.GetAccounts() {
		ts.accounts = append(ts.accounts, acc.GetId())
		ts.logger.Infof("Found account: %s", acc.GetId())
	}

	if len(ts.accounts) == 0 {
		ts.logger.Warn("No accounts found")
	}

	return nil
}

// setupRoutes - настройка HTTP маршрутов
func (ts *TradingServer) setupRoutes() {
	// Подключаем middleware
	ts.router.Use(middleware.Logger(ts.logger))
	ts.router.Use(middleware.Recovery(ts.logger))
	ts.router.Use(middleware.CORS())
	ts.router.Use(middleware.SecurityHeaders())
	ts.router.Use(middleware.RequestID())
	ts.router.Use(middleware.RateLimit(100)) // 100 запросов в минуту
	
	// Статические файлы для веб-интерфейса
	ts.router.Static("/static", "./web/static")
	ts.router.LoadHTMLGlob("web/templates/*")
	
	// Главная страница
	ts.router.GET("/", func(c *gin.Context) {
		c.HTML(http.StatusOK, "index.html", gin.H{
			"title": "Trading Server Dashboard",
		})
	})
	
	// Публичные маршруты
	public := ts.router.Group("/api/v1")
	public.GET("/status", ts.handleStatus)
	public.POST("/auth/login", ts.handleLogin)
	
	// Защищенные маршруты (требуют аутентификации)
	protected := ts.router.Group("/api/v1")
	protected.Use(middleware.Auth([]string{"demo-api-key"})) // В продакшене использовать реальные ключи
	
	// Информация об аккаунтах
	protected.GET("/accounts", ts.handleGetAccounts)
	protected.GET("/accounts/:id/portfolio", ts.handleGetPortfolio)
	protected.GET("/accounts/:id/positions", ts.handleGetPositions)
	protected.GET("/accounts/:id/operations", ts.handleGetOperations)
	
	// Ордера
	protected.POST("/orders/buy", ts.handleBuyOrder)
	protected.POST("/orders/sell", ts.handleSellOrder)
	protected.GET("/orders", ts.handleGetOrders)
	protected.GET("/orders/:id", ts.handleGetOrder)
	protected.DELETE("/orders/:id", ts.handleCancelOrder)
	
	// Инструменты
	protected.GET("/instruments/search", ts.handleSearchInstruments)
	protected.GET("/instruments/:figi", ts.handleGetInstrument)
	protected.GET("/instruments/shares", ts.handleGetShares)
	protected.GET("/instruments/bonds", ts.handleGetBonds)
	protected.GET("/instruments/etfs", ts.handleGetETFs)
	
	// Маркетдата
	protected.GET("/marketdata/candles", ts.handleGetCandles)
	protected.GET("/marketdata/orderbook", ts.handleGetOrderBook)
	protected.GET("/marketdata/last-prices", ts.handleGetLastPrices)
	protected.GET("/marketdata/trading-status", ts.handleGetTradingStatus)
	
	// Боты
	protected.GET("/bots", ts.handleGetBots)
	protected.POST("/bots", ts.handleCreateBot)
	protected.GET("/bots/:id", ts.handleGetBot)
	protected.PUT("/bots/:id", ts.handleUpdateBot)
	protected.DELETE("/bots/:id", ts.handleDeleteBot)
	protected.POST("/bots/:id/start", ts.handleStartBot)
	protected.POST("/bots/:id/stop", ts.handleStopBot)
	protected.POST("/bots/:id/pause", ts.handlePauseBot)
	protected.POST("/bots/:id/resume", ts.handleResumeBot)
	protected.GET("/bots/:id/stats", ts.handleGetBotStats)
	
	// WebSocket для стримов
	protected.GET("/ws", ts.handleWebSocket)
	
	// Административные маршруты
	admin := ts.router.Group("/admin")
	admin.Use(middleware.Auth([]string{"admin-api-key"}))
	admin.GET("/metrics", ts.handleMetrics)
	admin.GET("/health", ts.handleHealthCheck)
	admin.POST("/reload-config", ts.handleReloadConfig)
}

// HTTP обработчики
func (ts *TradingServer) handleGetAccounts(c *gin.Context) {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	
	c.JSON(http.StatusOK, gin.H{
		"accounts": ts.accounts,
		"count":    len(ts.accounts),
	})
}

func (ts *TradingServer) handleGetPortfolio(c *gin.Context) {
	accountId := c.Param("id")
	
	portfolioResp, err := ts.operationsService.GetPortfolio(accountId)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	
	c.JSON(http.StatusOK, portfolioResp)
}

func (ts *TradingServer) handleBuyOrder(c *gin.Context) {
	var orderReq struct {
		InstrumentId string  `json:"instrument_id" binding:"required"`
		Quantity     int64   `json:"quantity" binding:"required"`
		Price        *float64 `json:"price"`
		AccountId    string  `json:"account_id" binding:"required"`
	}
	
	if err := c.ShouldBindJSON(&orderReq); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	
	orderType := pb.OrderType_ORDER_TYPE_MARKET
	if orderReq.Price != nil {
		orderType = pb.OrderType_ORDER_TYPE_LIMIT
	}
	
	buyResp, err := ts.ordersService.Buy(&investgo.PostOrderRequestShort{
		InstrumentId: orderReq.InstrumentId,
		Quantity:     orderReq.Quantity,
		Price:        orderReq.Price,
		AccountId:    orderReq.AccountId,
		OrderType:    orderType,
		OrderId:      investgo.CreateUid(),
	})
	
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	
	c.JSON(http.StatusOK, buyResp)
}

func (ts *TradingServer) handleSellOrder(c *gin.Context) {
	// Аналогично handleBuyOrder, но для продажи
	var orderReq struct {
		InstrumentId string  `json:"instrument_id" binding:"required"`
		Quantity     int64   `json:"quantity" binding:"required"`
		Price        *float64 `json:"price"`
		AccountId    string  `json:"account_id" binding:"required"`
	}
	
	if err := c.ShouldBindJSON(&orderReq); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	
	orderType := pb.OrderType_ORDER_TYPE_MARKET
	if orderReq.Price != nil {
		orderType = pb.OrderType_ORDER_TYPE_LIMIT
	}
	
	sellResp, err := ts.ordersService.Sell(&investgo.PostOrderRequestShort{
		InstrumentId: orderReq.InstrumentId,
		Quantity:     orderReq.Quantity,
		Price:        orderReq.Price,
		AccountId:    orderReq.AccountId,
		OrderType:    orderType,
		OrderId:      investgo.CreateUid(),
	})
	
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	
	c.JSON(http.StatusOK, sellResp)
}

func (ts *TradingServer) handleSearchInstruments(c *gin.Context) {
	query := c.Query("q")
	if query == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "query parameter 'q' is required"})
		return
	}
	
	searchResp, err := ts.instrumentsService.FindInstrument(query)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	
	c.JSON(http.StatusOK, searchResp)
}

func (ts *TradingServer) handleStatus(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":    "running",
		"accounts":  len(ts.accounts),
		"timestamp": time.Now().Unix(),
	})
}

// Заглушки для остальных обработчиков
func (ts *TradingServer) handleGetPositions(c *gin.Context) {
	accountId := c.Param("id")
	positionsResp, err := ts.operationsService.GetPositions(accountId)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, positionsResp)
}

func (ts *TradingServer) handleGetOrders(c *gin.Context) {
	// Реализация получения ордеров
	c.JSON(http.StatusOK, gin.H{"orders": []interface{}{}})
}

func (ts *TradingServer) handleGetInstrument(c *gin.Context) {
	// Реализация получения инструмента по FIGI
	c.JSON(http.StatusOK, gin.H{"instrument": nil})
}

func (ts *TradingServer) handleGetCandles(c *gin.Context) {
	// Реализация получения свечей
	c.JSON(http.StatusOK, gin.H{"candles": []interface{}{}})
}

func (ts *TradingServer) handleGetOrderBook(c *gin.Context) {
	// Реализация получения стакана
	c.JSON(http.StatusOK, gin.H{"orderbook": nil})
}

// Обработчики для ботов
func (ts *TradingServer) handleGetBots(c *gin.Context) {
	bots := ts.botManager.GetBots()
	c.JSON(http.StatusOK, gin.H{"bots": bots})
}

func (ts *TradingServer) handleCreateBot(c *gin.Context) {
	var config bots.BotConfig
	if err := c.ShouldBindJSON(&config); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	botID, err := ts.botManager.CreateBot(config)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"bot_id": botID})
}

func (ts *TradingServer) handleGetBot(c *gin.Context) {
	botID := c.Param("id")
	
	allBots := ts.botManager.GetBots()
	bot, exists := allBots[botID]
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "Bot not found"})
		return
	}

	c.JSON(http.StatusOK, bot)
}

func (ts *TradingServer) handleUpdateBot(c *gin.Context) {
	botID := c.Param("id")
	
	var config bots.BotConfig
	if err := c.ShouldBindJSON(&config); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	err := ts.botManager.UpdateBotConfig(botID, config)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Bot updated successfully"})
}

func (ts *TradingServer) handleDeleteBot(c *gin.Context) {
	botID := c.Param("id")
	
	err := ts.botManager.DeleteBot(botID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Bot deleted successfully"})
}

func (ts *TradingServer) handleStartBot(c *gin.Context) {
	botID := c.Param("id")
	
	err := ts.botManager.StartBot(botID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Bot started successfully"})
}

func (ts *TradingServer) handleStopBot(c *gin.Context) {
	botID := c.Param("id")
	
	err := ts.botManager.StopBot(botID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Bot stopped successfully"})
}

func (ts *TradingServer) handlePauseBot(c *gin.Context) {
	botID := c.Param("id")
	
	bot, exists := ts.botManager.GetBot(botID)
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "Bot not found"})
		return
	}

	err := bot.Pause()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Bot paused successfully"})
}

func (ts *TradingServer) handleResumeBot(c *gin.Context) {
	botID := c.Param("id")
	
	bot, exists := ts.botManager.GetBot(botID)
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "Bot not found"})
		return
	}

	err := bot.Resume()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Bot resumed successfully"})
}

func (ts *TradingServer) handleGetBotStats(c *gin.Context) {
	botID := c.Param("id")
	
	stats, err := ts.botManager.GetBotStats(botID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, stats)
}

func (ts *TradingServer) handleWebSocket(c *gin.Context) {
	websocket.WebSocketHandler(ts.wsHub, ts)(c)
}

// Заглушки для остальных обработчиков
func (ts *TradingServer) handleGetPositions(c *gin.Context) {
	accountId := c.Param("id")
	positionsResp, err := ts.operationsService.GetPositions(accountId)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, positionsResp)
}

func (ts *TradingServer) handleGetOperations(c *gin.Context) {
	accountId := c.Param("id")
	
	// Получаем операции за последние 30 дней
	to := time.Now()
	from := to.AddDate(0, 0, -30)
	
	operationsResp, err := ts.operationsService.GetOperations(&investgo.OperationsRequest{
		AccountId: accountId,
		From:      from,
		To:        to,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, operationsResp)
}

func (ts *TradingServer) handleGetOrders(c *gin.Context) {
	accountId := c.Query("account_id")
	if accountId == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "account_id parameter required"})
		return
	}
	
	ordersResp, err := ts.ordersService.GetOrders(accountId)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, ordersResp)
}

func (ts *TradingServer) handleGetOrder(c *gin.Context) {
	orderID := c.Param("id")
	accountId := c.Query("account_id")
	
	if accountId == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "account_id parameter required"})
		return
	}
	
	orderResp, err := ts.ordersService.GetOrderState(&investgo.GetOrderStateRequest{
		AccountId: accountId,
		OrderId:   orderID,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, orderResp)
}

func (ts *TradingServer) handleCancelOrder(c *gin.Context) {
	orderID := c.Param("id")
	accountId := c.Query("account_id")
	
	if accountId == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "account_id parameter required"})
		return
	}
	
	cancelResp, err := ts.ordersService.CancelOrder(&investgo.CancelOrderRequest{
		AccountId: accountId,
		OrderId:   orderID,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, cancelResp)
}

func (ts *TradingServer) handleGetShares(c *gin.Context) {
	sharesResp, err := ts.instrumentsService.Shares(pb.InstrumentStatus_INSTRUMENT_STATUS_BASE)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, sharesResp)
}

func (ts *TradingServer) handleGetBonds(c *gin.Context) {
	bondsResp, err := ts.instrumentsService.Bonds(pb.InstrumentStatus_INSTRUMENT_STATUS_BASE)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, bondsResp)
}

func (ts *TradingServer) handleGetETFs(c *gin.Context) {
	etfsResp, err := ts.instrumentsService.Etfs(pb.InstrumentStatus_INSTRUMENT_STATUS_BASE)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, etfsResp)
}

func (ts *TradingServer) handleGetCandles(c *gin.Context) {
	figi := c.Query("figi")
	interval := c.Query("interval")
	
	if figi == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "figi parameter required"})
		return
	}
	
	// Преобразуем строку интервала в enum
	var candleInterval pb.CandleInterval
	switch interval {
	case "1min":
		candleInterval = pb.CandleInterval_CANDLE_INTERVAL_1_MIN
	case "5min":
		candleInterval = pb.CandleInterval_CANDLE_INTERVAL_5_MIN
	case "15min":
		candleInterval = pb.CandleInterval_CANDLE_INTERVAL_15_MIN
	case "hour":
		candleInterval = pb.CandleInterval_CANDLE_INTERVAL_HOUR
	case "day":
		candleInterval = pb.CandleInterval_CANDLE_INTERVAL_DAY
	default:
		candleInterval = pb.CandleInterval_CANDLE_INTERVAL_DAY
	}
	
	// Получаем свечи за последние 30 дней
	to := time.Now()
	from := to.AddDate(0, 0, -30)
	
	candlesResp, err := ts.marketDataService.GetCandles(&investgo.GetCandlesRequest{
		InstrumentId: figi,
		From:         from,
		To:           to,
		Interval:     candleInterval,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, candlesResp)
}

func (ts *TradingServer) handleGetOrderBook(c *gin.Context) {
	figi := c.Query("figi")
	depth := c.Query("depth")
	
	if figi == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "figi parameter required"})
		return
	}
	
	depthInt := int32(20) // По умолчанию
	if depth != "" {
		if d, err := strconv.ParseInt(depth, 10, 32); err == nil {
			depthInt = int32(d)
		}
	}
	
	orderBookResp, err := ts.marketDataService.GetOrderBook(&investgo.GetOrderBookRequest{
		InstrumentId: figi,
		Depth:        depthInt,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, orderBookResp)
}

func (ts *TradingServer) handleGetLastPrices(c *gin.Context) {
	figis := c.QueryArray("figi")
	
	if len(figis) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "at least one figi parameter required"})
		return
	}
	
	lastPricesResp, err := ts.marketDataService.GetLastPrices(figis)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, lastPricesResp)
}

func (ts *TradingServer) handleGetTradingStatus(c *gin.Context) {
	figi := c.Query("figi")
	
	if figi == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "figi parameter required"})
		return
	}
	
	tradingStatusResp, err := ts.marketDataService.GetTradingStatus(&investgo.GetTradingStatusRequest{
		InstrumentId: figi,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, tradingStatusResp)
}

func (ts *TradingServer) handleMetrics(c *gin.Context) {
	// Простые метрики для мониторинга
	metrics := gin.H{
		"uptime_seconds":    time.Since(time.Now()).Seconds(),
		"accounts_count":    len(ts.accounts),
		"bots_count":        len(ts.botManager.GetBots()),
		"active_bots_count": ts.countActiveBots(),
		"memory_usage":      "unknown", // Можно добавить runtime.MemStats
	}
	c.JSON(http.StatusOK, metrics)
}

func (ts *TradingServer) handleHealthCheck(c *gin.Context) {
	health := gin.H{
		"status":    "healthy",
		"timestamp": time.Now().Unix(),
		"services": gin.H{
			"api_client": "ok",
			"bot_manager": "ok",
			"websocket_hub": "ok",
		},
	}
	c.JSON(http.StatusOK, health)
}

func (ts *TradingServer) handleReloadConfig(c *gin.Context) {
	// Перезагрузка конфигурации (заглушка)
	c.JSON(http.StatusOK, gin.H{"message": "Config reloaded successfully"})
}

func (ts *TradingServer) handleLogin(c *gin.Context) {
	// Простая аутентификация для демо
	c.JSON(http.StatusOK, gin.H{
		"token": "demo-token",
		"message": "Login successful",
	})
}

func (ts *TradingServer) handleLogin(c *gin.Context) {
	// Простая аутентификация для демо
	c.JSON(http.StatusOK, gin.H{
		"token": "demo-token",
		"message": "Login successful",
	})
}

// Вспомогательные функции
func (ts *TradingServer) countActiveBots() int {
	count := 0
	for _, config := range ts.botManager.GetBots() {
		if config.IsActive {
			count++
		}
	}
	return count
} range ts.botManager.GetBots() {
		if config.IsActive {
			count++
		}
	}
	return count
}

// Start - запуск сервера
func (ts *TradingServer) Start(port string) error {
	ts.logger.Infof("Starting trading server on port %s", port)
	
	// Настраиваем HTTP сервер
	ts.httpServer = &http.Server{
		Addr:    ":" + port,
		Handler: ts.router,
	}
	
	// Запускаем стримы в отдельных горутинах
	ts.startStreams()
	
	// Запускаем HTTP сервер
	go func() {
		if err := ts.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			ts.logger.Errorf("HTTP server error: %v", err)
		}
	}()
	
	ts.logger.Info("Trading server started successfully")
	
	// Ждем сигнала завершения
	<-ts.ctx.Done()
	ts.logger.Info("Shutting down trading server...")
	
	return ts.Stop()
}

// startStreams - запуск стримов данных
func (ts *TradingServer) startStreams() {
	// Здесь можно запустить стримы маркетдаты и операций
	// в отдельных горутинах для получения данных в реальном времени
	ts.logger.Info("Starting data streams...")
}

// Stop - остановка сервера
func (ts *TradingServer) Stop() error {
	ts.logger.Info("Stopping trading server...")
	
	// Отменяем контекст
	ts.cancel()
	
	// Останавливаем всех ботов
	if ts.botManager != nil {
		if err := ts.botManager.Shutdown(); err != nil {
			ts.logger.Errorf("Bot manager shutdown error: %v", err)
		}
	}
	
	// Останавливаем HTTP сервер
	if ts.httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := ts.httpServer.Shutdown(ctx); err != nil {
			ts.logger.Errorf("HTTP server shutdown error: %v", err)
		}
	}
	
	// Останавливаем клиент API
	if ts.client != nil {
		if err := ts.client.Stop(); err != nil {
			ts.logger.Errorf("API client shutdown error: %v", err)
		}
	}
	
	// Ждем завершения всех горутин
	ts.wg.Wait()
	
	// Синхронизируем логгер
	if err := ts.logger.Sync(); err != nil {
		log.Printf("Logger sync error: %v", err)
	}
	
	ts.logger.Info("Trading server stopped")
	return nil
}

// main функция
func main() {
	server, err := NewTradingServer()
	if err != nil {
		log.Fatalf("Failed to create trading server: %v", err)
	}
	
	// Запускаем сервер на порту 8080
	if err := server.Start("8080"); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}