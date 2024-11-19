package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/golang/groupcache/lru"
	"github.com/sony/gobreaker"
	"golang.org/x/time/rate"
)

type Metrics struct {
	Requests     int
	Errors       int
	LatencyTotal time.Duration
}

type GlobalMetrics struct {
	TotalRequests int
	TotalErrors   int
	AvgLatency    time.Duration
}

type TLSConfig struct {
	Enabled  bool   `json:"enabled"`
	CertFile string `json:"certFile"`
	KeyFile  string `json:"keyFile"`
}

type CacheConfig struct {
	Enabled bool          `json:"enabled"`
	MaxSize int           `json:"maxSize"`
	TTL     time.Duration `json:"ttl"`
}

type cacheConfigJSON struct {
	Enabled bool   `json:"enabled"`
	MaxSize int    `json:"maxSize"`
	TTL     string `json:"ttl"`
}

func (c *CacheConfig) UnmarshalJSON(data []byte) error {
	var jsonConfig cacheConfigJSON
	if err := json.Unmarshal(data, &jsonConfig); err != nil {
		return err
	}

	c.Enabled = jsonConfig.Enabled
	c.MaxSize = jsonConfig.MaxSize

	if jsonConfig.TTL != "" {
		ttl, err := time.ParseDuration(jsonConfig.TTL)
		if err != nil {
			return fmt.Errorf("erro ao fazer parse do TTL do cache: %v", err)
		}
		c.TTL = ttl
	}

	return nil
}

type RateLimiterConfig struct {
	Enabled           bool    `json:"enabled"`
	RequestsPerSecond float64 `json:"requestsPerSecond"`
	Burst             int     `json:"burst"`
}

type CircuitBreakerConfig struct {
	Enabled     bool          `json:"enabled"`
	MaxRequests uint32        `json:"maxRequests"`
	Interval    time.Duration `json:"interval"`
	Timeout     time.Duration `json:"timeout"`
}

type circuitBreakerConfigJSON struct {
	Enabled     bool   `json:"enabled"`
	MaxRequests uint32 `json:"maxRequests"`
	Interval    string `json:"interval"`
	Timeout     string `json:"timeout"`
}

func (c *CircuitBreakerConfig) UnmarshalJSON(data []byte) error {
	var jsonConfig circuitBreakerConfigJSON
	if err := json.Unmarshal(data, &jsonConfig); err != nil {
		return err
	}

	c.Enabled = jsonConfig.Enabled
	c.MaxRequests = jsonConfig.MaxRequests

	if jsonConfig.Interval != "" {
		interval, err := time.ParseDuration(jsonConfig.Interval)
		if err != nil {
			return fmt.Errorf("erro ao fazer parse do interval do circuit breaker: %v", err)
		}
		c.Interval = interval
	}

	if jsonConfig.Timeout != "" {
		timeout, err := time.ParseDuration(jsonConfig.Timeout)
		if err != nil {
			return fmt.Errorf("erro ao fazer parse do timeout do circuit breaker: %v", err)
		}
		c.Timeout = timeout
	}

	return nil
}

type SessionConfig struct {
	Enabled    bool          `json:"enabled"`
	CookieName string        `json:"cookieName"`
	TTL        time.Duration `json:"ttl"`
}

type sessionConfigJSON struct {
	Enabled    bool   `json:"enabled"`
	CookieName string `json:"cookieName"`
	TTL        string `json:"ttl"`
}

func (s *SessionConfig) UnmarshalJSON(data []byte) error {
	var jsonConfig sessionConfigJSON
	if err := json.Unmarshal(data, &jsonConfig); err != nil {
		return err
	}

	s.Enabled = jsonConfig.Enabled
	s.CookieName = jsonConfig.CookieName

	if jsonConfig.TTL != "" {
		ttl, err := time.ParseDuration(jsonConfig.TTL)
		if err != nil {
			return fmt.Errorf("erro ao fazer parse do TTL da sessão: %v", err)
		}
		s.TTL = ttl
	}

	return nil
}

type SessionManager struct {
	cookieName string
	ttl        time.Duration
	sessions   sync.Map
}

type CacheItem struct {
	Content    []byte
	Expiration time.Time
	headers    http.Header
}

type Config struct {
	Servers             []ServerConfig       `json:"servers"`
	Algorithm           string               `json:"algorithm"`
	HealthCheckInterval time.Duration        `json:"healthCheckInterval"`
	EnableRetry         bool                 `json:"enableRetry"`
	MaxRetries          int                  `json:"maxRetries"`
	Timeout             time.Duration        `json:"timeout"`
	RateLimit           RateLimiterConfig    `json:"rateLimit"`
	Cache               CacheConfig          `json:"cache"`
	Session             SessionConfig        `json:"session"`
	CircuitBreaker      CircuitBreakerConfig `json:"circuitBreaker"`
	Compression         struct {
		Enabled bool `json:"enabled"`
	} `json:"compression"`
	TLS TLSConfig `json:"tls"`
}

type Server struct {
	URL                 *url.URL
	Alive               bool
	Weight              int
	CurrentLoad         int64
	Metrics             *Metrics
	mux                 sync.RWMutex
	ReverseProxy        *httputil.ReverseProxy
	HealthCheckURL      string
	FailureThreshold    int
	FailureCount        int
	Limiter             *rate.Limiter
	CircuitBreaker      *gobreaker.CircuitBreaker
	mutex               sync.Mutex
	successfulRequests  int64
	failedRequests      int64
	totalResponseTime   time.Duration
	averageResponseTime time.Duration
}

type CacheEntry struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
	Timestamp  time.Time
}

type LoadBalancer struct {
	servers             []*Server
	currentIndex        int // Adicionar este campo se não existir
	mutex               sync.Mutex
	algorithm           string
	healthCheckEnabled  bool
	healthCheckInterval time.Duration
	retryEnabled        bool
	maxRetries          int
	timeout             time.Duration
	metrics             *GlobalMetrics
	cache               *lru.Cache
	sessionManager      *SessionManager
	compression         bool
	tlsConfig           *tls.Config
}

type ConfigJSON struct {
	Servers             []ServerConfig       `json:"servers"`
	Algorithm           string               `json:"algorithm"`
	HealthCheckInterval string               `json:"healthCheckInterval"`
	EnableRetry         bool                 `json:"enableRetry"`
	MaxRetries          int                  `json:"maxRetries"`
	Timeout             string               `json:"timeout"`
	RateLimit           RateLimiterConfig    `json:"rateLimit"`
	Cache               CacheConfig          `json:"cache"`
	Session             SessionConfig        `json:"session"`
	CircuitBreaker      CircuitBreakerConfig `json:"circuitBreaker"`
	Compression         struct {
		Enabled bool `json:"enabled"`
	} `json:"compression"`
	TLS TLSConfig `json:"tls"`
}

func (sm *SessionManager) setSession(w http.ResponseWriter, r *http.Request, server *Server) {
	sessionID := generateSessionID()
	sm.sessions.Store(sessionID, server)

	http.SetCookie(w, &http.Cookie{
		Name:    sm.cookieName,
		Value:   sessionID,
		Expires: time.Now().Add(sm.ttl),
	})
}

func (sm *SessionManager) getServer(sessionID string) *Server {
	if server, ok := sm.sessions.Load(sessionID); ok {
		return server.(*Server)
	}
	return nil
}

func generateSessionID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func loadConfig(configPath string) (*Config, error) {
	file, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("erro ao ler arquivo de configuração: %v", err)
	}

	var configJSON ConfigJSON
	if err := json.Unmarshal(file, &configJSON); err != nil {
		return nil, fmt.Errorf("erro ao fazer parse do JSON de configuração: %v", err)
	}

	// Converte as strings de duração para time.Duration
	healthCheckInterval, err := time.ParseDuration(configJSON.HealthCheckInterval)
	if err != nil {
		return nil, fmt.Errorf("erro ao fazer parse do healthCheckInterval: %v", err)
	}

	timeout, err := time.ParseDuration(configJSON.Timeout)
	if err != nil {
		return nil, fmt.Errorf("erro ao fazer parse do timeout: %v", err)
	}

	config := &Config{
		Servers:             configJSON.Servers,
		Algorithm:           configJSON.Algorithm,
		HealthCheckInterval: healthCheckInterval,
		EnableRetry:         configJSON.EnableRetry,
		MaxRetries:          configJSON.MaxRetries,
		Timeout:             timeout,
		RateLimit:           configJSON.RateLimit,
		Cache:               configJSON.Cache,
		Session:             configJSON.Session,
		CircuitBreaker:      configJSON.CircuitBreaker,
		Compression:         configJSON.Compression,
		TLS:                 configJSON.TLS,
	}

	// Validações básicas
	if len(config.Servers) == 0 {
		return nil, fmt.Errorf("pelo menos um servidor deve ser configurado")
	}

	if config.Algorithm == "" {
		config.Algorithm = "round-robin"
	}

	if config.HealthCheckInterval == 0 {
		config.HealthCheckInterval = 30 * time.Second
	}

	if config.Timeout == 0 {
		config.Timeout = 30 * time.Second
	}

	if config.MaxRetries == 0 {
		config.MaxRetries = 3
	}

	return config, nil
}

func NewLoadBalancer(configPath string) (*LoadBalancer, error) {
	config, err := loadConfig(configPath)
	if err != nil {
		return nil, err
	}

	// Initialize cache if enabled
	var cache *lru.Cache
	if config.Cache.Enabled {
		cache = lru.New(config.Cache.MaxSize)
	}

	// Initialize TLS if enabled
	var tlsConfig *tls.Config
	if config.TLS.Enabled {
		cert, err := tls.LoadX509KeyPair(config.TLS.CertFile, config.TLS.KeyFile)
		if err != nil {
			return nil, err
		}
		tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
	}

	lb := &LoadBalancer{
		algorithm:           config.Algorithm,
		healthCheckEnabled:  true,
		healthCheckInterval: config.HealthCheckInterval,
		retryEnabled:        config.EnableRetry,
		maxRetries:          config.MaxRetries,
		timeout:             config.Timeout,
		metrics:             &GlobalMetrics{},
		cache:               cache,
		compression:         config.Compression.Enabled,
		tlsConfig:           tlsConfig,
		currentIndex:        0, // Inicializar o índice
	}

	// Initialize session manager if sticky sessions are enabled
	if config.Session.Enabled {
		lb.sessionManager = &SessionManager{
			cookieName: config.Session.CookieName,
			ttl:        config.Session.TTL, // Já está no formato time.Duration
		}
	}

	// Initialize servers with rate limiters and circuit breakers
	for _, serverConfig := range config.Servers {
		server, err := lb.createServer(serverConfig, config.RateLimit, config.CircuitBreaker)
		if err != nil {
			return nil, err
		}
		lb.servers = append(lb.servers, server)
	}

	return lb, nil
}

func (lb *LoadBalancer) createServer(config ServerConfig, rateLimitConfig RateLimiterConfig, cbConfig CircuitBreakerConfig) (*Server, error) {
	url, err := url.Parse(config.URL)
	if err != nil {
		return nil, err
	}

	// Configure rate limiter
	var limiter *rate.Limiter
	if rateLimitConfig.Enabled {
		limiter = rate.NewLimiter(rate.Limit(rateLimitConfig.RequestsPerSecond), rateLimitConfig.Burst)
	}

	// Configure circuit breaker
	var cb *gobreaker.CircuitBreaker
	if cbConfig.Enabled {
		cb = gobreaker.NewCircuitBreaker(gobreaker.Settings{
			Name:        fmt.Sprintf("CB-%s", url.Host),
			MaxRequests: cbConfig.MaxRequests,
			Interval:    cbConfig.Interval,
			Timeout:     cbConfig.Timeout,
			ReadyToTrip: func(counts gobreaker.Counts) bool {
				failureRatio := float64(counts.TotalFailures) / float64(counts.Requests)
				return counts.Requests >= 3 && failureRatio >= 0.6
			},
		})
	}

	// Configure proxy with middleware
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = url.Scheme
			req.URL.Host = url.Host
			req.Host = url.Host
		},
		ModifyResponse: func(resp *http.Response) error {
			// Add compression if enabled
			if lb.compression && !isCompressed(resp) {
				return lb.compressResponse(resp)
			}
			return nil
		},
	}

	return &Server{
		URL:              url,
		Alive:            true,
		Weight:           config.Weight,
		HealthCheckURL:   config.HealthCheckURL,
		FailureThreshold: config.FailureThreshold,
		ReverseProxy:     proxy,
		Limiter:          limiter,
		CircuitBreaker:   cb,
	}, nil
}

type ServerConfig struct {
	URL              string `json:"url"`
	Weight           int    `json:"weight"`
	HealthCheckURL   string `json:"healthCheckURL"`
	FailureThreshold int    `json:"failureThreshold"`
}

type responseWriter struct {
	http.ResponseWriter
	statusCode int
	body       []byte
}

func (rw *responseWriter) WriteHeader(statusCode int) {
	rw.statusCode = statusCode
	rw.ResponseWriter.WriteHeader(statusCode)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	if rw.statusCode == 0 {
		rw.statusCode = http.StatusOK
	}
	rw.body = append(rw.body, b...)
	return rw.ResponseWriter.Write(b)
}

// updateMetrics atualiza as métricas do servidor
func (lb *LoadBalancer) updateMetrics(server *Server, success bool, duration time.Duration) {
	server.mutex.Lock()
	defer server.mutex.Unlock()

	if success {
		server.successfulRequests++
		server.totalResponseTime += duration
		server.averageResponseTime = server.totalResponseTime / time.Duration(server.successfulRequests)
	} else {
		server.failedRequests++
	}
}

// isCacheable verifica se uma resposta pode ser cacheada
func isCacheable(r *http.Request, statusCode int) bool {
	// Só cacheia requisições GET
	if r.Method != http.MethodGet {
		return false
	}

	// Só cacheia respostas bem-sucedidas
	if statusCode < 200 || statusCode >= 300 {
		return false
	}

	// Verifica headers de cache
	if r.Header.Get("Cache-Control") == "no-cache" ||
		r.Header.Get("Cache-Control") == "no-store" ||
		r.Header.Get("Pragma") == "no-cache" {
		return false
	}

	return true
}

func (lb *LoadBalancer) getServerForSession(w http.ResponseWriter, r *http.Request) *Server {
	if lb.sessionManager == nil {
		return lb.getNextServer()
	}

	cookie, err := r.Cookie(lb.sessionManager.cookieName)
	if err != nil {
		server := lb.getNextServer()
		if server != nil {
			lb.sessionManager.setSession(w, r, server)
		}
		return server
	}

	if server := lb.sessionManager.getServer(cookie.Value); server != nil {
		return server
	}

	server := lb.getNextServer()
	if server != nil {
		lb.sessionManager.setSession(w, r, server)
	}
	return server
}

func (lb *LoadBalancer) cacheResponse(r *http.Request, rw *responseWriter) {
	if lb.cache == nil {
		return
	}

	cacheKey := r.URL.String()
	cacheEntry := CacheEntry{
		StatusCode: rw.statusCode,
		Headers:    rw.Header(),
		Body:       rw.body,
		Timestamp:  time.Now(),
	}

	lb.cache.Add(cacheKey, cacheEntry)
}

func (lb *LoadBalancer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if lb.cache != nil {
		if cachedResponse, ok := lb.getFromCache(r); ok {
			lb.serveCachedResponse(w, r, cachedResponse)
			return
		}
	}

	server := lb.getServerForSession(w, r)
	if server == nil {
		http.Error(w, "No server available", http.StatusServiceUnavailable)
		return
	}

	if server.Limiter != nil {
		if !server.Limiter.Allow() {
			http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
			return
		}
	}

	if server.CircuitBreaker != nil {
		_, err := server.CircuitBreaker.Execute(func() (interface{}, error) {
			return nil, lb.handleRequest(server, w, r)
		})

		if err != nil {
			http.Error(w, "Circuit breaker error", http.StatusServiceUnavailable)
			return
		}

	} else {
		if err := lb.handleRequest(server, w, r); err != nil {
			http.Error(w, "Request handling error", http.StatusInternalServerError)
			return
		}
	}
}

func (lb *LoadBalancer) handleRequest(server *Server, w http.ResponseWriter, r *http.Request) error {
	// Capture the response for caching
	rw := &responseWriter{ResponseWriter: w}

	start := time.Now()
	server.ReverseProxy.ServeHTTP(rw, r)
	duration := time.Since(start)

	// Update metrics
	lb.updateMetrics(server, rw.statusCode >= 200 && rw.statusCode < 500, duration)

	// Cache the response if appropriate
	if lb.cache != nil && isCacheable(r, rw.statusCode) {
		lb.cacheResponse(r, rw)
	}

	return nil
}

func (lb *LoadBalancer) compressResponse(resp *http.Response) error {
	body := resp.Body
	defer body.Close()

	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	if _, err := io.Copy(gz, body); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}

	resp.Body = ioutil.NopCloser(&b)
	resp.Header.Set("Content-Encoding", "gzip")
	resp.Header.Del("Content-Length")
	return nil
}

func (lb *LoadBalancer) startHealthChecks() {
	go func() {
		ticker := time.NewTicker(lb.healthCheckInterval)
		for range ticker.C {
			for _, server := range lb.servers {
				go lb.checkHealth(server)
			}
		}
	}()
}

func (lb *LoadBalancer) checkHealth(server *Server) {
	client := &http.Client{
		Timeout: 5 * time.Second,
	}

	url := fmt.Sprintf("%s%s", server.URL.String(), server.HealthCheckURL)
	resp, err := client.Get(url)

	server.mux.Lock()
	defer server.mux.Unlock()

	if err != nil || resp.StatusCode != http.StatusOK {
		server.FailureCount++
		if server.FailureCount >= server.FailureThreshold {
			server.Alive = false
		}
	} else {
		server.FailureCount = 0
		server.Alive = true
	}

	if resp != nil {
		resp.Body.Close()
	}
}

func (lb *LoadBalancer) startAdminServer(addr string) {
	adminMux := http.NewServeMux()

	// Endpoint para métricas
	adminMux.HandleFunc("/metrics", lb.handleMetrics)
	// Endpoint para status dos servidores
	adminMux.HandleFunc("/status", lb.handleStatus)

	go func() {
		if err := http.ListenAndServe(addr, adminMux); err != nil {
			log.Printf("Erro ao iniciar servidor admin: %v", err)
		}
	}()
}

func (lb *LoadBalancer) handleMetrics(w http.ResponseWriter, r *http.Request) {
	metrics := map[string]interface{}{
		"totalRequests": lb.metrics.TotalRequests,
		"totalErrors":   lb.metrics.TotalErrors,
		"avgLatency":    lb.metrics.AvgLatency.String(),
	}

	json.NewEncoder(w).Encode(metrics)
}

func (lb *LoadBalancer) handleStatus(w http.ResponseWriter, r *http.Request) {
	status := make([]map[string]interface{}, len(lb.servers))
	for i, server := range lb.servers {
		status[i] = map[string]interface{}{
			"url":   server.URL.String(),
			"alive": server.Alive,
			"load":  server.CurrentLoad,
		}
	}

	json.NewEncoder(w).Encode(status)
}

func (lb *LoadBalancer) getFromCache(r *http.Request) (*CacheEntry, bool) {
	if lb.cache == nil {
		return nil, false
	}

	cacheKey := r.URL.String()
	if cached, ok := lb.cache.Get(cacheKey); ok {
		entry := cached.(CacheEntry)
		if time.Since(entry.Timestamp) < time.Duration(lb.cache.MaxEntries) {
			return &entry, true
		}
		lb.cache.Remove(cacheKey)
	}
	return nil, false
}

// serveCachedResponse envia uma resposta cacheada para o cliente
func (lb *LoadBalancer) serveCachedResponse(w http.ResponseWriter, r *http.Request, cache *CacheEntry) {
	for k, v := range cache.Headers {
		w.Header()[k] = v
	}
	w.WriteHeader(cache.StatusCode)
	w.Write(cache.Body)
}

// getNextServer seleciona o próximo servidor baseado no algoritmo configurado
func (lb *LoadBalancer) getNextServer() *Server {
	lb.mutex.Lock()
	defer lb.mutex.Unlock()

	// Adicionar contador para round-robin
	if len(lb.servers) == 0 {
		return nil
	}

	// Implementação correta do round-robin
	nextServer := lb.servers[lb.currentIndex]
	lb.currentIndex = (lb.currentIndex + 1) % len(lb.servers)

	// Procura próximo servidor vivo
	for i := 0; i < len(lb.servers); i++ {
		if nextServer.Alive {
			return nextServer
		}
		nextServer = lb.servers[lb.currentIndex]
		lb.currentIndex = (lb.currentIndex + 1) % len(lb.servers)
	}

	return nil
}

// isCompressed verifica se a resposta já está comprimida
func isCompressed(resp *http.Response) bool {
	return resp.Header.Get("Content-Encoding") != ""
}

func main() {
	lb, err := NewLoadBalancer("config.json")
	if err != nil {
		log.Fatal(err)
	}

	// Start health checks
	lb.startHealthChecks()

	// Start admin server
	lb.startAdminServer(":8081")

	server := &http.Server{
		Addr:      ":8080",
		Handler:   lb,
		TLSConfig: lb.tlsConfig,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("Load Balancer started on port 8080")
		if lb.tlsConfig != nil {
			if err := server.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
				log.Fatal(err)
			}
		} else {
			if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatal(err)
			}
		}
	}()

	<-stop

	log.Println("Shutting down Load Balancer...")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Fatalf("Load Balancer forced to shutdown: %v", err)
	}
}
