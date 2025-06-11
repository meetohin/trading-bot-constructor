package middleware

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// Logger - middleware для логирования запросов
func Logger(logger *zap.SugaredLogger) gin.HandlerFunc {
	return gin.HandlerFunc(func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		raw := c.Request.URL.RawQuery

		// Обрабатываем запрос
		c.Next()

		// Логируем результат
		param := gin.LogFormatterParams{
			Request:    c.Request,
			TimeStamp:  time.Now(),
			Latency:    time.Since(start),
			ClientIP:   c.ClientIP(),
			Method:     c.Request.Method,
			StatusCode: c.Writer.Status(),
			ErrorMessage: c.Errors.ByType(gin.ErrorTypePrivate).String(),
			BodySize:   c.Writer.Size(),
			Keys:       c.Keys,
		}

		if raw != "" {
			param.Path = path + "?" + raw
		} else {
			param.Path = path
		}

		logger.Infow("HTTP Request",
			"method", param.Method,
			"path", param.Path,
			"status", param.StatusCode,
			"latency", param.Latency,
			"ip", param.ClientIP,
			"user_agent", c.Request.UserAgent(),
			"body_size", param.BodySize,
			"error", param.ErrorMessage,
		)
	})
}

// CORS - middleware для настройки CORS
func CORS() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		c.Writer.Header().Set("Access-Control-Allow-Credentials", "true")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization, accept, origin, Cache-Control, X-Requested-With")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS, GET, PUT, DELETE")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}

		c.Next()
	}
}

// RateLimit - middleware для ограничения частоты запросов
func RateLimit(requestsPerMinute int) gin.HandlerFunc {
	// Простая реализация rate limiting в памяти
	// В продакшене лучше использовать Redis
	clients := make(map[string][]time.Time)
	
	return func(c *gin.Context) {
		clientIP := c.ClientIP()
		now := time.Now()
		
		// Очищаем старые записи
		if requests, exists := clients[clientIP]; exists {
			var validRequests []time.Time
			for _, reqTime := range requests {
				if now.Sub(reqTime) < time.Minute {
					validRequests = append(validRequests, reqTime)
				}
			}
			clients[clientIP] = validRequests
		}
		
		// Проверяем лимит
		if len(clients[clientIP]) >= requestsPerMinute {
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error": "Rate limit exceeded",
				"retry_after": 60,
			})
			c.Abort()
			return
		}
		
		// Добавляем текущий запрос
		clients[clientIP] = append(clients[clientIP], now)
		
		c.Next()
	}
}

// Auth - middleware для проверки аутентификации
func Auth(validAPIKeys []string) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Проверяем API ключ в заголовке
		apiKey := c.GetHeader("X-API-Key")
		if apiKey != "" {
			for _, validKey := range validAPIKeys {
				if apiKey == validKey {
					c.Next()
					return
				}
			}
		}
		
		// Проверяем Bearer token
		authHeader := c.GetHeader("Authorization")
		if authHeader != "" {
			parts := strings.Split(authHeader, " ")
			if len(parts) == 2 && parts[0] == "Bearer" {
				// Здесь должна быть проверка JWT токена
				token := parts[1]
				if validateJWT(token) {
					c.Next()
					return
				}
			}
		}
		
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": "Unauthorized",
			"message": "Valid API key or Bearer token required",
		})
		c.Abort()
	}
}

// validateJWT - проверка JWT токена (заглушка)
func validateJWT(token string) bool {
	// Здесь должна быть реальная проверка JWT
	// Для примера просто проверяем, что токен не пустой
	return token != ""
}

// Recovery - middleware для обработки паники
func Recovery(logger *zap.SugaredLogger) gin.HandlerFunc {
	return gin.CustomRecovery(func(c *gin.Context, recovered interface{}) {
		err, ok := recovered.(string)
		if !ok {
			err = fmt.Sprintf("%v", recovered)
		}
		
		logger.Errorw("Panic recovered",
			"error", err,
			"path", c.Request.URL.Path,
			"method", c.Request.Method,
			"ip", c.ClientIP(),
		)
		
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Internal server error",
			"message": "The server encountered an unexpected error",
		})
	})
}

// RequestID - middleware для добавления уникального ID запроса
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := generateRequestID()
		c.Header("X-Request-ID", requestID)
		c.Set("request_id", requestID)
		c.Next()
	}
}

// generateRequestID - генерация уникального ID запроса
func generateRequestID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

// SecurityHeaders - middleware для добавления заголовков безопасности
func SecurityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("X-XSS-Protection", "1; mode=block")
		c.Header("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		c.Header("Content-Security-Policy", "default-src 'self'")
		c.Next()
	}
}

// Timeout - middleware для установки таймаута запроса
func Timeout(timeout time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Создаем контекст с таймаутом
		ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
		defer cancel()
		
		// Заменяем контекст запроса
		c.Request = c.Request.WithContext(ctx)
		
		// Канал для отслеживания завершения
		finished := make(chan struct{})
		
		go func() {
			c.Next()
			close(finished)
		}()
		
		select {
		case <-ctx.Done():
			c.JSON(http.StatusRequestTimeout, gin.H{
				"error": "Request timeout",
				"message": "The request took too long to process",
			})
			c.Abort()
		case <-finished:
			// Запрос завершен нормально
		}
	}
}