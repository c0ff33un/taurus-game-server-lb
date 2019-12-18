package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	Attempts int = iota
	Retry
)

// ServerPool holds information about reachable backends

// GetAttemptsFromContext returns the attempts for request
func GetAttemptsFromContext(r *http.Request) int {
	if attempts, ok := r.Context().Value(Attempts).(int); ok {
		return attempts
	}
	return 1
}

// GetAttemptsFromContext returns the attempts for request
func GetRetryFromContext(r *http.Request) int {
	if retry, ok := r.Context().Value(Retry).(int); ok {
		return retry
	}
	return 0
}

var apiPrefix string = os.Getenv("API_PREFIX")

var roomAction, roomConnection *regexp.Regexp = regexp.MustCompile(`/room/[0-9]+(/.+)*`), regexp.MustCompile(`/ws/[0-9]`)

// lb load balances the incoming request
func lb(w http.ResponseWriter, r *http.Request) {
	attempts := GetAttemptsFromContext(r)
	if attempts > 3 {
		log.Printf("%s(%s) Max attempts reached, terminating\n", r.RemoteAddr, r.URL.Path)
		http.Error(w, "Service not available", http.StatusServiceUnavailable)
		return
	}
	path := r.URL.Path
	log.Printf("Incoming Request: %v\n", path)
	// Load Balance Room Creation Request!
	if path == apiPrefix+"/room" {
		peer := serverPool.GetNextPeer()
		if peer != nil {
			peer.ServeHTTP(w, r)
			return
		}
	} else {
		//Route other requests
		var scheme string
		switch {
		case roomAction.MatchString(path):
			scheme = "ws"
		case roomConnection.MatchString(path):
			scheme = "http"
		default:
			log.Println("URL doesn't match any resource")
			http.Error(w, "URL doesn't match any resource", http.StatusNotFound)
			return
		}
		s := strings.Split(path, "/")
		roomId, _ := strconv.Atoi(s[2])
		log.Printf("roomId: %v", roomId)
		peer := serverPool.GetPeer(roomId)
		if peer == nil {
			log.Println("Server doesn't exists")
			http.Error(w, "Server doesn't exists", http.StatusServiceUnavailable)
			return
		}
		switch scheme {
		case "ws":
			peer.ServeWS(w, r)
		case "http":
			peer.ServeHTTP(w, r)
		}
	}
	log.Println("Service not available")
	http.Error(w, "Service not available", http.StatusServiceUnavailable)
}

// isAlive checks whether a backend is Alive by establishing a TCP connection
func isBackendAlive(u *url.URL) bool {
	timeout := 2 * time.Second
	conn, err := net.DialTimeout("tcp", u.Host, timeout)
	if err != nil {
		log.Println("Site unreachable, error: ", err)
		return false
	}
	_ = conn.Close()
	return true
}

// healthCheck runs a routine for check status of the backends every 2 mins
func healthCheck() {
	t := time.NewTicker(time.Second * 20)
	for {
		select {
		case <-t.C:
			log.Println("Starting health check...")
			serverPool.HealthCheck()
			log.Println("Health check completed")
		}
	}
}

var serverPool ServerPool

func createProxy(u *url.URL) *httputil.ReverseProxy {
	proxy := httputil.NewSingleHostReverseProxy(u)
	proxy.ErrorHandler = func(writer http.ResponseWriter, request *http.Request, e error) {
		log.Printf("[%s] %s\n", u.Host, e.Error())
		retries := GetRetryFromContext(request)
		if retries < 3 {
			select {
			case <-time.After(10 * time.Millisecond):
				ctx := context.WithValue(request.Context(), Retry, retries+1)
				proxy.ServeHTTP(writer, request.WithContext(ctx))
			}
			return
		}

		// after 3 retries, mark this backend as down
		serverPool.MarkBackendStatus(u, false)

		// if the same request routing for few attempts with different backends, increase the count
		attempts := GetAttemptsFromContext(request)
		log.Printf("%s(%s) Attempting retry %d\n", request.RemoteAddr, request.URL.Path, attempts)
		ctx := context.WithValue(request.Context(), Attempts, attempts+1)
		lb(writer, request.WithContext(ctx))
	}
	return proxy
}

func getSecure() string {
	if os.Getenv("SECURE_LAYER") != "" {
		return "s"
	}
	return ""
}

func main() {
	var serverList string
	var port int
	serverList = os.Getenv("SERVER_LIST")
	//flag.StringVar(&serverList, "backends", "", "Load balanced backends, use commas to separate")
	flag.IntVar(&port, "port", 3030, "Port to serve")
	flag.Parse()

	if len(serverList) == 0 {
		log.Fatal("Please provide one or more backends to load balance")
	}

	// parse servers
	tokens := strings.Split(serverList, ",")
	for _, tok := range tokens {
		log.Printf("Try add Backend: %v", tok)
		serverUrl, err := url.Parse("http" + getSecure() + "://" + tok)
		if err != nil {
			log.Fatal(err)
		}
		wsServerUrl, err := url.Parse("ws" + getSecure() + "://" + tok)
		if err != nil {
			log.Fatal(err)
		}

		serverPool.AddBackend(&Backend{
			URL:            serverUrl,
			Alive:          true,
			ReverseProxy:   createProxy(serverUrl),
			WsReverseProxy: createProxy(wsServerUrl),
		})
		log.Printf("Configured server: %s\n", serverUrl)
		log.Printf("Configured server: %s\n", wsServerUrl)
	}

	// create http server
	server := http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: http.HandlerFunc(lb),
	}

	// start health checking
	//go healthCheck()

	log.Printf("Load Balancer started at :%d\n", port)
	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
