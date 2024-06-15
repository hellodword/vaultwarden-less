package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sync"

	"gopkg.in/mcuadros/go-syslog.v2"
)

type Log struct {
	Status        int    `json:"status"`
	RequestMethod string `json:"request_method"`
	RequestURI    string `json:"request_uri"`
}

func (l *Log) Format() string {
	var uri []byte
	for _, b := range []byte(l.RequestURI) {
		if b == '/' || ('0' <= b && '9' <= b) || ('a' <= b && 'z' <= b) || ('A' <= b && 'Z' <= b) {
			uri = append(uri, b)
		} else {
			break
		}
	}
	return fmt.Sprintf("(%s)(%d)(%s)", l.RequestMethod, l.Status, string(uri))
}

type Config struct {
	ExcludeRequestURI       []string         `json:"exclude_request_uri"`
	ExcludeRequestURIRegexp []*regexp.Regexp `json:"-"`
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
	for _, uri := range c.ExcludeRequestURI {
		r := regexp.MustCompile(uri)
		r.MatchString("/")
		c.ExcludeRequestURIRegexp = append(c.ExcludeRequestURIRegexp, r)
	}

	return &c, nil
}

func main() {
	configFile := flag.String("config", "syslog-parser.json", "")
	flag.Parse()

	config, err := loadConfig(*configFile)
	if err != nil {
		panic(err)
	}

	channel := make(syslog.LogPartsChannel)
	handler := syslog.NewChannelHandler(channel)

	server := syslog.NewServer()
	server.SetFormat(syslog.RFC3164)
	server.SetHandler(handler)

	addr := fmt.Sprintf("%s:%d", "0.0.0.0", 50333)
	// It seems that Nginx syslog:server= does not support `ListenTCP`
	err = server.ListenUDP(addr)
	if err != nil {
		panic(err)
	}

	err = server.Boot()
	if err != nil {
		panic(err)
	}

	go func(channel syslog.LogPartsChannel) {
		var lock sync.Mutex

		for logParts := range channel {
			if logParts == nil {
				continue
			}

			v, ok := logParts["content"]
			if !ok {
				continue
			}

			content, ok := v.(string)
			if !ok {
				continue
			}

			var parsedLog Log
			err = json.Unmarshal([]byte(content), &parsedLog)
			if err != nil {
				log.Println(err)
				continue
			}

			// GET won't change anything
			if parsedLog.RequestMethod != http.MethodPost &&
				parsedLog.RequestMethod != http.MethodPut &&
				parsedLog.RequestMethod != http.MethodDelete {
				continue
			}

			// don't care about failed requests
			if parsedLog.Status < http.StatusOK || parsedLog.Status >= http.StatusMultipleChoices {
				continue
			}

			// exclude changes
			var excluded = false
			for _, r := range config.ExcludeRequestURIRegexp {
				if r.MatchString(parsedLog.RequestURI) {
					excluded = true
					break
				}
			}
			if excluded {
				continue
			}

			// TODO concurrent
			go func() {
				lock.Lock()
				defer lock.Unlock()
				onChange(&parsedLog)
			}()

		}
	}(channel)

	server.Wait()
}

func onChange(parsedLog *Log) {
	log.Printf("%+v\n", *parsedLog)

	output, err := backup()
	if err != nil {
		msg := fmt.Sprintf("%s failed: %s", parsedLog.Format(), err.Error())
		log.Println(msg)
		notify(output + "\n" + msg)
		return
	}

	msg := fmt.Sprintf("%s succeed", parsedLog.Format())
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
