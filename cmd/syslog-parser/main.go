package main

import (
	"encoding/json"
	"flag"
	"fmt"
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

	err := backupToLocal()
	if err != nil {
		msg := "backupToLocal failed: " + err.Error()
		log.Println(msg)
		notify(msg)
		return
	}

	err = backupToRemote()
	if err != nil {
		msg := "backupToRemote failed: " + err.Error()
		log.Println(msg)
		notify(msg)
		return
	}

	notify("backup succeed")

}

func backupToLocal() error {
	cmd := exec.Command("/scripts/backup-to-local")
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	err := cmd.Run()
	if err != nil {
		return err
	}
	return nil
}

func backupToRemote() error {
	cmd := exec.Command("/scripts/backup-to-remote")
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	err := cmd.Run()
	if err != nil {
		return err
	}
	return nil
}

func notify(msg string) error {
	cmd := exec.Command("/scripts/notify",
		"vaultwarden-less", // group
		"vaultwarden-less", // title
		msg,                // desc
	)
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	err := cmd.Run()
	if err != nil {
		return err
	}
	return nil
}
