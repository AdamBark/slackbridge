package slack

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"sync"

	"github.com/matrix-org/slackbridge/common"

	"golang.org/x/net/websocket"
)

const bufSize = 16 * 1024

type MessageFilter func(*Message) bool

func AlwaysNotify(m *Message) bool {
	return true
}

func NewClient(token string, c http.Client, messageFilter MessageFilter) *client {
	return &client{
		token:          token,
		client:         c,
		messageFilter:  messageFilter,
		asUser:         "",
		echoSuppresser: common.NewEchoSuppresser(),
	}
}

func NewBotClient(token, asUser, displayName, avatarURL string, c http.Client, messageFilter MessageFilter) *client {
	return &client{
		token:          token,
		client:         c,
		messageFilter:  messageFilter,
		asUser:         asUser,
		displayName:    displayName,
		avatarURL:      avatarURL,
		echoSuppresser: common.NewEchoSuppresser(),
	}
}

func (c *client) Listen(cancel chan struct{}) error {
	if c.ws != nil {
		return fmt.Errorf("already listening")
	}

	url, err := c.websocketURL()
	if err != nil {
		return err
	}
	c.ws, err = websocket.Dial(url, "", "http://localhost")
	if err != nil {
		return fmt.Errorf("error dialing: %v", err)
	}

	ch := make(chan []byte)
	for {
		go c.read(ch)
		select {
		case b := <-ch:
			var e event
			if err := json.Unmarshal(b, &e); err != nil {
				log.Printf("Error unmarshaling websocket type: %v", err)
			}
			switch e.Type {
			case "hello":
				var h Hello
				if err := json.Unmarshal(b, &h); err != nil {
					log.Printf("Error unmarshaling websocket response: %v", err)
				}
				if len(c.helloHandlers) == 0 {
					log.Printf("No listeners for hello events")
				}
				for _, c := range c.helloHandlers {
					c(h)
				}
			case "message":
				var m Message
				if err := json.Unmarshal(b, &m); err != nil {
					log.Printf("Error unmarshaling websocket response: %v", err)
				}
				c.echoSuppresser.Wait()
				if !c.messageFilter(&m) || c.echoSuppresser.WasSent(m.TS) {
					log.Printf("Skipping filtered message: %v", m)
					continue
				}
				if len(c.messageHandlers) == 0 {
					log.Printf("No listeners for message events")
				}
				for _, c := range c.messageHandlers {
					c(m)
				}
			default:
				log.Printf("Ignoring unknown event: %q", string(b))
			}
		case _ = <-cancel:
			return c.ws.Close()
		}
	}
	return nil
}

func (c *client) OnHello(h func(Hello)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.helloHandlers = append(c.helloHandlers, h)
}

func (c *client) OnMessage(h func(Message)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messageHandlers = append(c.messageHandlers, h)
}

// Technically you can use the websocket to send pure text-only messages, but
// you can't send richer messages like attachments through the websocket, so
// we will instead consistently use the HTTP API.
func (c *client) SendText(channelID, text string) error {
	v := url.Values{}
	v.Set("text", text)
	return c.sendMessage(channelID, v)
}

func (c *client) SendImage(channelID, fallbackText, imageURL string) error {
	v := url.Values{}
	attachments, err := json.Marshal([]map[string]string{
		map[string]string{
			"fallback":  fallbackText,
			"image_url": imageURL,
		},
	})
	if err != nil {
		return fmt.Errorf("error json encoding attachments: %v", err)
	}
	v.Set("attachments", string(attachments))
	return c.sendMessage(channelID, v)
}

func (c *client) sendMessage(channelID string, v url.Values) error {
	v.Set("token", c.token)
	v.Set("channel", channelID)
	if c.asUser == "" {
		v.Set("as_user", "true")
	} else {
		v.Set("as_user", "false")
		if c.displayName == "" {
			v.Set("username", c.asUser)
		} else {
			v.Set("username", c.displayName)
		}
		if c.avatarURL != "" {
			v.Set("icon_url", c.avatarURL)
		}
	}
	c.echoSuppresser.StartSending()
	defer c.echoSuppresser.DoneSending()
	resp, err := c.client.PostForm("https://slack.com/api/chat.postMessage", v)
	if err != nil {
		return fmt.Errorf("error from slack: %v", err)
	}
	defer resp.Body.Close()
	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("error reading response from slack: %v", err)
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("error from slack: %d: %s", resp.StatusCode, string(b))
	}
	var sr slackResponse
	if err := json.Unmarshal(b, &sr); err != nil {
		return fmt.Errorf("error decoding JSON from slack: %v (%v)", err, b)
	}
	if !sr.OK {
		return fmt.Errorf("error from slack: %s", string(b))
	}
	c.echoSuppresser.Sent(sr.TS)

	return nil
}

func (c *client) AccessToken() string {
	return c.token
}

type client struct {
	token       string
	asUser      string
	displayName string
	avatarURL   string
	client      http.Client
	ws          *websocket.Conn

	mu              sync.Mutex
	helloHandlers   []func(Hello)
	messageHandlers []func(Message)

	messageFilter  MessageFilter
	echoSuppresser *common.EchoSuppresser
}

func (c *client) websocketURL() (string, error) {
	resp, err := c.client.Get("https://slack.com/api/rtm.start?token=" + c.token)
	if err != nil {
		return "", fmt.Errorf("error starting stream: %v", err)
	}
	defer resp.Body.Close()
	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("error reading stream details: %v", err)
	}
	var r rtmStartResponse
	if err := json.Unmarshal(b, &r); err != nil {
		return "", fmt.Errorf("error unmarshaling response: %v", err)
	}
	if !r.OK {
		log.Printf("Bad response from slack getting websocket: %v", string(b))
		return "", fmt.Errorf("bad response: %v", err)
	}
	return r.URL, nil
}

func (c *client) read(ch chan []byte) {
	b := make([]byte, bufSize)
	n, err := c.ws.Read(b)
	if err != nil {
		log.Printf("Error reading from websocket: %v", err)
	}
	ch <- b[0:n]
}

type rtmStartResponse struct {
	OK  bool   `json:"ok"`
	URL string `json:"url"`
}

type slackResponse struct {
	OK bool   `json:"ok"`
	TS string `json:"ts"`
}
