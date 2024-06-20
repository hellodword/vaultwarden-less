package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Config struct {
	Upstream            string   `json:"upstream"`
	Addr                string   `json:"addr"`
	ExcludePath         []string `json:"exclude_path"`
	IncludeMethod       []string `json:"include_method"`
	Script              Script   `json:"script"`
	VerboseNotification bool     `json:"verbose_notification"`
}

type Script struct {
	Backup string `json:"backup"`
	Notify string `json:"notify"`
}

func loadConfig(configFile string) (*Config, error) {
	b, err := os.ReadFile(configFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var c Config
	err = json.Unmarshal(b, &c)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	// Verify patterns
	for _, pattern := range c.ExcludePath {
		if _, err := regexp.Compile(pattern); err != nil {
			return nil, fmt.Errorf("invalid exclude_path pattern %s: %w", pattern, err)
		}
	}

	return &c, nil
}

func main() {
	configFile := flag.String("config", "trigger.json", "")
	flag.Parse()

	config, err := loadConfig(*configFile)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	log.Printf("loaded config: %+v\n", config)

	var wgConsumer, wgProducer sync.WaitGroup
	var taskCh = make(chan string)
	var quitCh = make(chan struct{})

	upstream, err := url.Parse(config.Upstream)
	if err != nil || upstream.String() == "" {
		log.Fatalf("invalid upstream URL: %v", err)
	}

	proxy := httputil.NewSingleHostReverseProxy(upstream)
	proxy.ModifyResponse = func(response *http.Response) (_ error) {
		// nil r.Request? i dont care
		method, path, status := response.Request.Method, response.Request.URL.Path, response.StatusCode

		// exclude failed requests
		if status < http.StatusOK || status >= http.StatusMultipleChoices {
			return
		}

		if !slices.Contains(config.IncludeMethod, method) || isExcludedPath(config.ExcludePath, path) {
			return
		}

		task := formatRequest(method, path, status)

		wgProducer.Add(1)
		go func() {
			defer wgProducer.Done()
			log.Println("queueing task:", task)
			defer log.Println("queued task", task)
			taskCh <- task
		}()

		return
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { proxy.ServeHTTP(w, r) })
	server := &http.Server{Addr: config.Addr, Handler: nil}

	go func() {
		log.Println("listening on", config.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("server error: %v\n", err)
		}

		close(quitCh)
	}()

	wgConsumer.Add(1)
	go func() {
		defer wgConsumer.Done()
		for task := range taskCh {
			handleTask(task, config)
		}
	}()

	notifier := make(chan os.Signal, 1)
	signal.Notify(notifier, os.Interrupt, syscall.SIGTERM)

	select {
	case sig := <-notifier:
		log.Println("received sig", sig.String())
		break
	case <-quitCh:
		break
	}

	// by default, docker compose will forcefully kill the container after 10 seconds,
	// if there is no task in the queue, this notification won't be successfully executed,
	// a better way is `docker compose stop/down -t 300`
	go execute(config.Script.Notify, "shutting down")
	shutdownServer(server)

	// make sure all the tasks been handled
	wgProducer.Wait()
	close(taskCh)
	wgConsumer.Wait()
}

func isExcludedPath(patterns []string, path string) bool {
	for _, pattern := range patterns {
		if regexp.MustCompile(pattern).MatchString(path) {
			return true
		}
	}
	return false
}

func formatRequest(method, path string, status int) string {
	safePath := strings.Map(func(b rune) rune {
		if b == '/' || ('0' <= b && b <= '9') || ('a' <= b && b <= 'z') || ('A' <= b && b <= 'Z') {
			return b
		}
		return '-'
	}, path)
	return fmt.Sprintf("(%s)(%d)(%s)", method, status, safePath)
}

func handleTask(task string, config *Config) {
	log.Println("handling task", task)
	defer log.Println("handled task", task)

	output, err := execute(config.Script.Backup)
	if err != nil {
		msg := fmt.Sprintf("%s failed: %s", task, err.Error())
		log.Println(msg)
		if config.VerboseNotification {
			msg = output + "\n" + msg
		}
		execute(config.Script.Notify, msg)
		return
	}

	msg := fmt.Sprintf("%s succeed", task)
	log.Println(msg)
	execute(config.Script.Notify, msg)
}

func execute(name string, arg ...string) (string, error) {
	cmd := exec.Command(name, arg...)
	var output bytes.Buffer
	cmd.Stdout = io.MultiWriter(os.Stdout, &output)
	cmd.Stderr = io.MultiWriter(os.Stderr, &output)
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("command execution failed: %w", err)
	}
	return output.String(), nil
}

func shutdownServer(server *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Println("server shutdown error:", err)
	} else {
		log.Println("server gracefully stopped")
	}
}
