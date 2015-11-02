package discord

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

const (
	apiBase     = "https://discordapp.com/api"
	apiGateway  = apiBase + "/gateway"
	apiUsers    = apiBase + "/users"
	apiRegister = apiBase + "/auth/register"
	apiLogin    = apiBase + "/auth/login"
	apiLogout   = apiBase + "/auth/logout"
	apiServers  = apiBase + "/guilds"
	apiChannels = apiBase + "/channels"
)

// Client is the main object, instantiate it to use Discord Websocket API
type Client struct {
	OnReady                func(Ready)
	OnMessageCreate        func(Message)
	OnTypingStart          func(Typing)
	OnPresenceUpdate       func(Presence)
	OnChannelCreate        func(Channel)
	OnPrivateChannelCreate func(PrivateChannel)
	OnChannelDelete        func(Channel)
	OnPrivateChannelDelete func(PrivateChannel)

	// Print websocket dumps (may be huge)
	Debug bool

	Servers         map[string]Server
	PrivateChannels map[string]PrivateChannel

	user    User
	wsConn  *websocket.Conn
	gateway string
	token   string
}

func doRequest(req *http.Request) (interface{}, error) {
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// JSON from payload
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var reqResult = map[string]interface{}{}
	if err := json.Unmarshal(body, &reqResult); err != nil {
		return nil, err
	}

	return reqResult, nil
}

func (c *Client) doHandshake() {
	log.Print("Sending handshake")
	c.wsConn.WriteJSON(map[string]interface{}{
		"op": 2,
		"d": map[string]interface{}{
			"token": c.token,
			"properties": map[string]string{
				"$os":               "linux",
				"$browser":          "go-discord",
				"$device":           "go-discord",
				"$referer":          "",
				"$referring_domain": "",
			},
			"v": 3,
		},
	})
}

func (c *Client) initServers(ready Ready) {
	c.Servers = make(map[string]Server)
	c.PrivateChannels = make(map[string]PrivateChannel)
	for _, server := range ready.Servers {
		c.Servers[server.ID] = server
	}
	for _, private := range ready.PrivateChannels {
		c.PrivateChannels[private.ID] = private
	}
}

func (c *Client) handleReady(eventStr []byte) {
	var ready readyEvent
	if err := json.Unmarshal(eventStr, &ready); err != nil {
		log.Printf("handleReady: %s", err)
		return
	}

	// WebSocket keepalive
	go func() {
		ticker := time.NewTicker(ready.Data.HeartbeatInterval * time.Millisecond)
		for range ticker.C {
			timestamp := int(time.Now().Unix())
			log.Printf("Sending keepalive with timestamp %d", timestamp)
			c.wsConn.WriteJSON(map[string]int{
				"op": 1,
				"d":  timestamp,
			})
		}
	}()

	c.user = ready.Data.User
	c.initServers(ready.Data)

	if c.OnReady == nil {
		log.Print("No handler for READY")
	} else {
		log.Print("Client ready, calling OnReady handler")
		c.OnReady(ready.Data)
	}
}

func (c *Client) handleMessageCreate(eventStr []byte) {
	if c.OnMessageCreate == nil {
		log.Print("No handler for MESSAGE_CREATE")
		return
	}

	var message messageEvent
	if err := json.Unmarshal(eventStr, &message); err != nil {
		log.Printf("messageCreate: %s", err)
		return
	}

	if message.Data.Author.ID != c.user.ID {
		c.OnMessageCreate(message.Data)
	} else {
		log.Print("Ignoring message from self")
	}
}

func (c *Client) handleTypingStart(eventStr []byte) {
	if c.OnTypingStart == nil {
		log.Print("No handler for TYPING_START")
		return
	}

	var typing typingEvent
	if err := json.Unmarshal(eventStr, &typing); err != nil {
		log.Printf("typingStart: %s", err)
		return
	}

	c.OnTypingStart(typing.Data)
}

func (c *Client) handlePresenceUpdate(eventStr []byte) {
	if c.OnPresenceUpdate == nil {
		log.Print("No handler for PRESENCE_UPDATE")
		return
	}

	var presence presenceEvent
	if err := json.Unmarshal(eventStr, &presence); err != nil {
		log.Printf("presenceUpdate: %s", err)
		return
	}

	c.OnPresenceUpdate(presence.Data)
}

func (c *Client) handleChannelCreate(eventStr []byte) {
	var channelCreate interface{}
	if err := json.Unmarshal(eventStr, &channelCreate); err != nil {
		log.Printf("handleChannelCreate: %s", err)
		return
	}

	// woot
	isPrivate := channelCreate.(map[string]interface{})["d"].(map[string]interface{})["is_private"].(bool)

	if isPrivate {
		var event privateChannelCreateEvent
		if err := json.Unmarshal(eventStr, &event); err != nil {
			log.Printf("privateChannelCreate: %s", err)
			return
		}

		privateChannel := event.Data
		c.PrivateChannels[privateChannel.ID] = privateChannel

		if c.OnPrivateChannelCreate == nil {
			log.Print("No handler for private CHANNEL_CREATE")
		} else {
			c.OnPrivateChannelCreate(privateChannel)
		}
	} else {
		var event channelCreateEvent
		if err := json.Unmarshal(eventStr, &event); err != nil {
			log.Printf("channelCreate: %s", err)
			return
		}

		channel := event.Data
		// XXX: Workaround for c.Channels[private.ID].Private = true
		// https://github.com/golang/go/issues/3117
		tmp := c.Servers[channel.ServerID]
		tmp.Channels = append(tmp.Channels, channel)
		c.Servers[channel.ServerID] = tmp

		if c.OnChannelCreate == nil {
			log.Print("No handler for CHANNEL_CREATE")
		} else {
			c.OnChannelCreate(channel)
		}
	}
}

func (c *Client) handleChannelDelete(eventStr []byte) {
	var channelDelete interface{}
	if err := json.Unmarshal(eventStr, &channelDelete); err != nil {
		log.Printf("handleChannelDelete: %s", err)
		return
	}

	// woot #2
	isPrivate := channelDelete.(map[string]interface{})["d"].(map[string]interface{})["is_private"].(bool)

	if isPrivate {
		var privateChannel PrivateChannel
		if err := json.Unmarshal(eventStr, &privateChannel); err != nil {
			log.Printf("privateChannelCreate: %s", err)
			return
		}
		delete(c.PrivateChannels, privateChannel.ID)
		if c.OnPrivateChannelDelete == nil {
			log.Print("No handler for private CHANNEL_DELETE")
		} else {
			c.OnPrivateChannelDelete(privateChannel)
		}
	} else {
		var channel Channel
		if err := json.Unmarshal(eventStr, &channel); err != nil {
			log.Printf("channelDelete: %s", err)
			return
		}

		// Get channel id in slice of server
		i, _ := c.GetChannelByID(channel.ID)
		// XXX: Workaround for c.Channels[private.ID].Private = true
		// https://github.com/golang/go/issues/3117
		tmp := c.Servers[channel.ServerID]
		tmp.Channels = append(tmp.Channels[:i], tmp.Channels[i+1:]...)
		c.Servers[channel.ServerID] = tmp

		if c.OnChannelDelete == nil {
			log.Print("No handler for CHANNEL_DELETE")
		} else {
			c.OnChannelDelete(channel)
		}
	}
}

func (c *Client) handleEvent(eventStr []byte) {
	var event interface{}
	if err := json.Unmarshal(eventStr, &event); err != nil {
		log.Print(err)
		return
	}

	eventType := event.(map[string]interface{})["t"].(string)

	if c.Debug {
		log.Printf("%s : %s", eventType, string(eventStr[:]))
	}

	// TODO: There must be a better way to directly cast the eventStr
	// to its corresponding object, avoiding double-unmarshal
	switch eventType {
	case "READY":
		c.handleReady(eventStr)
	case "MESSAGE_CREATE":
		c.handleMessageCreate(eventStr)
	case "TYPING_START":
		c.handleTypingStart(eventStr)
	case "PRESENCE_UPDATE":
		c.handlePresenceUpdate(eventStr)
	case "CHANNEL_CREATE":
		c.handleChannelCreate(eventStr)
	case "CHANNEL_DELETE":
		c.handleChannelDelete(eventStr)
	default:
		log.Printf("Ignoring %s", eventType)
		log.Printf("event dump: %s", string(eventStr[:]))
	}

}

// Get sends a GET request to the given url
func (c *Client) get(url string) (interface{}, error) {
	// Prepare request
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", c.token)

	return doRequest(req)
}

// Post sends a POST request with payload to the given url
func (c *Client) request(method string, url string, payload interface{}) (interface{}, error) {
	payloadJSON, _ := json.Marshal(payload)
	contentReader := bytes.NewReader(payloadJSON)

	// Prepare request
	req, err := http.NewRequest(method, url, contentReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", c.token)
	req.Header.Set("Content-Type", "application/json")

	return doRequest(req)
}

// Login initialize Discord connection by requesting a token
func (c *Client) Login(email string, password string) error {
	// Prepare POST json
	m := map[string]string{
		"email":    email,
		"password": password,
	}

	// Get token
	tokenResp, err := c.request("POST", apiLogin, m)
	if err != nil {
		return err
	}
	c.token = tokenResp.(map[string]interface{})["token"].(string)

	// Get websocket gateway
	gatewayResp, err := c.get(apiGateway)
	if err != nil {
		return err
	}
	c.gateway = gatewayResp.(map[string]interface{})["url"].(string)

	return nil
}

// LoginFromFile call login with email and password found in the given file
func (c *Client) LoginFromFile(filename string) error {
	fileDump, err := ioutil.ReadFile(filename)
	if err != nil {
		return err
	}

	type fileCredentials struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}

	var creds = fileCredentials{}
	if err := json.Unmarshal(fileDump, &creds); err != nil {
		return err
	}

	return c.Login(creds.Email, creds.Password)
}

// GetChannel returns the Channel object from the given channel name on the given server name
func (c *Client) GetChannel(serverName string, channelName string) Channel {
	var res Channel
	server := c.GetServer(serverName)
	for _, channel := range server.Channels {
		if channel.Name == channelName {
			res = channel
			break
		}
	}
	return res
}

// GetChannelByID returns the Channel object from the given ID as well as its position in its server's channel list
func (c *Client) GetChannelByID(channelID string) (int, Channel) {
	var channelPos int
	var res Channel
	for _, server := range c.Servers {
		for i, channel := range server.Channels {
			if channel.ID == channelID {
				channelPos = i
				res = channel
				break
			}
		}
	}
	return channelPos, res
}

// GetServer returns the Server object from the given server name
func (c *Client) GetServer(serverName string) Server {
	var res Server
	for _, server := range c.Servers {
		if server.Name == serverName {
			res = server
			break
		}
	}
	return res
}

// GetUserByID returns the User object from the given user ID
func (c *Client) GetUserByID(userID string) User {
	var res User
	for _, server := range c.Servers {
		for _, member := range server.Members {
			if member.User.ID == userID {
				res = member.User
				break
			}
		}
	}
	return res
}

// SendMessage sends a message to the given channel
// XXX: string sent as channel ID because of Channel/PrivateChannel differences
func (c *Client) SendMessage(channelID string, content string) error {
	_, err := c.request(
		"POST",
		fmt.Sprintf(apiChannels+"/%s/messages", channelID),
		map[string]string{
			"content": content,
		},
	)
	if err != nil {
		log.Printf("Failed to send message to %s", channelID)
	} else {
		log.Printf("Message sent to %s", channelID)
	}
	return err
}

// SendMessageMention sends a message to the given channel mentionning users
// XXX: string sent as channel ID because of Channel/PrivateChannel differences
func (c *Client) SendMessageMention(channelID string, content string, mentions []User) error {

	var userMentions []string
	for _, user := range mentions {
		userMentions = append(userMentions, user.ID)
	}

	_, err := c.request(
		"POST",
		fmt.Sprintf(apiChannels+"/%s/messages", channelID),
		map[string]interface{}{
			"content":  content,
			"mentions": mentions,
		},
	)
	if err != nil {
		log.Printf("Failed to send message to %s", channelID)
	} else {
		log.Printf("Message sent to %s", channelID)
	}
	return err
}

// Ban bans a user from the giver server
func (c *Client) Ban(server Server, user User) error {
	response, err := c.request(
		"PUT",
		fmt.Sprintf("%s/%s/bans/%s", apiServers, server.ID, user.ID),
		nil,
	)
	if c.Debug {
		log.Print(response)
	}
	return err
}

// Unban unbans a user from the giver server
func (c *Client) Unban(server Server, user User) error {
	response, err := c.request(
		"DELETE",
		fmt.Sprintf("%s/%s/bans/%s", apiServers, server.ID, user.ID),
		nil,
	)
	if c.Debug {
		log.Print(response)
	}
	return err
}

// Run init the WebSocket connection and starts listening on it
func (c *Client) Run() {
	log.Printf("Setting up websocket to %s", c.gateway)
	conn, _, err := websocket.DefaultDialer.Dial(c.gateway, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	log.Print("Connected")
	c.wsConn = conn

	c.doHandshake()

	for {
		_, message, err := c.wsConn.ReadMessage()
		if err != nil {
			log.Print(err)
			break
		}
		go c.handleEvent(message)
	}
}

// Stop closes the WebSocket connection
func (c *Client) Stop() {
	log.Print("Closing connection")
	c.wsConn.Close()
}
