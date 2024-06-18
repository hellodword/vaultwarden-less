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
	"sync"
	"syscall"
)

func NewProxy(upstream string) (*httputil.ReverseProxy, error) {
	u, err := url.Parse(upstream)
	if err != nil {
		return nil, err
	}

	if u.String() == "" {
		return nil, fmt.Errorf("invalid upstream %s", upstream)
	}

	return httputil.NewSingleHostReverseProxy(u), nil
}

func ProxyRequestHandler(proxy *httputil.ReverseProxy) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		proxy.ServeHTTP(w, r)
	}
}

type Config struct {
	Upstream string `json:"upstream"`
	Addr     string `json:"addr"`
	// https://pkg.go.dev/regexp
	ExcludePath   []string `json:"exclude_path"`
	IncludeMethod []string `json:"include_method"`
}

func loadConfig(configFile string) (*Config, error) {
	b, err := os.ReadFile(configFile)
	if err != nil {
		return nil, err
	}

	var c Config
	err = json.Unmarshal(b, &c)
	if err != nil {
		return nil, err
	}

	// verify
	for _, pattern := range c.ExcludePath {
		regexp.MustCompile(pattern).MatchString("/")
	}

	return &c, nil
}

func main() {
	configFile := flag.String("config", "trigger.json", "")
	flag.Parse()

	config, err := loadConfig(*configFile)
	if err != nil {
		panic(err)
	}

	log.Printf("config %+v\n", config)

	proxy, err := NewProxy(config.Upstream)
	if err != nil {
		panic(err)
	}

	var wgConsumer, wgProducer sync.WaitGroup
	var taskCh = make(chan string)
	var quitCh = make(chan struct{})

	wgConsumer.Add(1)
	go func() {
		defer wgConsumer.Done()
		for task := range taskCh {
			onChange(task)
		}
	}()

	proxy.ModifyResponse = func(response *http.Response) error {
		// nil r.Request? i dont care
		method, path, status := response.Request.Method, response.Request.URL.Path, response.StatusCode

		// exclude failed requests
		if status < http.StatusOK || status >= http.StatusMultipleChoices {
			return nil
		}

		// only POST PUT DELETE will modify database
		if !slices.Contains(config.IncludeMethod, method) {
			return nil
		}

		// exclude changes
		var excluded = false
		for _, pattern := range config.ExcludePath {
			if regexp.MustCompile(pattern).MatchString(path) {
				excluded = true
				break
			}
		}
		if excluded {
			return nil
		}

		formatted := formatRequest(method, path, status)

		// TODO concurrent
		wgProducer.Add(1)
		go func() {
			defer wgProducer.Done()
			log.Println("adding", formatted)
			defer log.Println("added", formatted)

			taskCh <- formatted
		}()

		return nil
	}

	http.HandleFunc("/", ProxyRequestHandler(proxy))
	server := &http.Server{Addr: config.Addr, Handler: nil}
	go func() {
		log.Println("listening on", config.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Println("server err", err)
		}

		close(quitCh)
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

	// by default, docker compose will forcefully kill the container after 10 seconds, so notify when shutting down
	// a better way is `docker compose stop/down -t 300`
	go notify("shutting down")
	server.Shutdown(context.Background())

	// make sure all the tasks been handled
	wgProducer.Wait()
	close(taskCh)
	wgConsumer.Wait()
}

func formatRequest(method, path string, status int) string {
	var uri []byte
	for _, b := range []byte(path) {
		if b == '/' || ('0' <= b && '9' <= b) || ('a' <= b && 'z' <= b) || ('A' <= b && 'Z' <= b) {
			uri = append(uri, b)
		} else {
			break
		}
	}
	return fmt.Sprintf("(%s)(%d)(%s)", method, status, string(uri))
}

func onChange(formatted string) {
	log.Println("handling task", formatted)
	defer log.Println("handled task", formatted)

	output, err := backup()
	if err != nil {
		msg := fmt.Sprintf("%s failed: %s", formatted, err.Error())
		log.Println(msg)
		notify(output + "\n" + msg)
		return
	}

	msg := fmt.Sprintf("%s succeed", formatted)
	log.Println(msg)
	notify(msg)
}

func backup() (string, error) {
	cmd := exec.Command("/scripts/backup")
	var output bytes.Buffer
	cmd.Stdout = io.MultiWriter(os.Stdout, &output)
	cmd.Stderr = io.MultiWriter(os.Stderr, &output)
	err := cmd.Run()
	if err != nil {
		return output.String(), err
	}
	return output.String(), nil
}

func notify(msg string) (string, error) {
	cmd := exec.Command("/scripts/notify", msg)
	var output bytes.Buffer
	cmd.Stdout = io.MultiWriter(os.Stdout, &output)
	cmd.Stderr = io.MultiWriter(os.Stderr, &output)
	err := cmd.Run()
	if err != nil {
		return output.String(), err
	}
	return output.String(), nil
}
