package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"expvar"
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/kelseyhightower/envconfig"
	zmq "github.com/pebbe/zmq2"
	"golang.org/x/net/websocket"
)

// obviously, this should not be hard-coded in real life:
var SECRET = "6f1d916c-7761-4874-8d5b-8f8f93d20bf2"

var AUTH_WINDOW = 60 * time.Second

var startTime = time.Now().UTC()

// metrics

// messages from the broker
var numMessages = expvar.NewInt("NumZMQMessages")

// currently connected clients
var numClients = expvar.NewInt("NumClients")

// total clients that have been seen
var totalClients = expvar.NewInt("TotalClients")

// bundle a list of online users along with
// in and out channels
type room struct {
	Users     map[*OnlineUser]bool
	Broadcast chan envelope
	Incoming  chan envelope
}

// how we route zmq messages around
type envelope struct {
	Address string `json:"address"`
	Content string `json:"content"`
}

func uptime() interface{} {
	uptime := time.Since(startTime)
	return int64(uptime)
}

func startswith(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	return s[:len(prefix)] == prefix
}

func (e envelope) RouteTo(u *OnlineUser) bool {
	return startswith(e.Address, u.Uci.SubPrefix)
}

var runningRoom *room = &room{}

// listen for messages on a channel
// and route them out them to appropriate users
func (r *room) run() {
	log.Debug("room.run()")
	for e := range r.Broadcast {
		for u := range r.Users {
			if e.RouteTo(u) {
				u.Send <- e
			}
		}
	}
}

func (r *room) SendMessage(e envelope) {
	r.Broadcast <- e
}

func InitRoom() {
	log.Debug("InitRoom()")
	runningRoom = &room{
		Users:     make(map[*OnlineUser]bool),
		Broadcast: make(chan envelope),
		Incoming:  make(chan envelope),
	}
	go runningRoom.run()
}

type OnlineUser struct {
	Connection *websocket.Conn
	Uci        userConnectionInfo
	Send       chan envelope
}

// loop indefinitely, taking messages on a channel
// and sending them out to the user's websocket
func (this *OnlineUser) PushToClient() {
	for e := range this.Send {
		err := websocket.JSON.Send(this.Connection, e)
		log.Info("sent websocket message")
		if err != nil {
			break
		}
	}
}

// loop indefinitely listening for incoming
// messages from a user's websocket
func (this *OnlineUser) PullFromClient() {
	for {
		var content string
		err := websocket.Message.Receive(this.Connection, &content)

		if err != nil {
			return
		}
		log.Info("incoming:", content)
		runningRoom.Incoming <- envelope{this.Uci.PubPrefix, content}
	}
}

type userConnectionInfo struct {
	Uni       string
	SubPrefix string
	PubPrefix string
}

// improvements that should be made:
// * include hash function name in the token (so we can swap it in the future)
// * include a version number in the token (to enable backwards compatability)
// * allow a mode where IP address isn't checked

func validateToken(token string, current_time time.Time,
	remote_ip net.Addr, uci *userConnectionInfo) error {
	// token will look something like this:
	// anp8:gobot:gobot.browser.anp8:1344361884:667494:127.0.0.1:306233f64522f1f970fc62fb3cf2d7320c899851
	parts := strings.Split(token, ":")
	if len(parts) != 7 {
		log.WithFields(
			log.Fields{
				"token": token,
				"parts": len(parts),
			}).Error("couldn't parse token")
		return errors.New("invalid token")
	}
	// their UNI
	uni := parts[0]
	sub_prefix := parts[1]
	pub_prefix := parts[2]
	uci.Uni = uni
	uci.SubPrefix = sub_prefix
	uci.PubPrefix = pub_prefix
	// UNIX timestamp
	now, err := strconv.Atoi(parts[3])
	if err != nil {
		log.WithFields(log.Fields{
			"token":           token,
			"timestamp_field": parts[3],
			"now":             now,
			"error":           err,
		}).Error("invalid timestamp in token")
		return errors.New("invalid timestamp in token")
	}
	// a random salt
	salt := parts[4]
	ip_address := parts[5]
	// the hmac of those parts with our shared secret
	hmc := parts[6]
	// make sure we're within a 60 second window
	token_time := time.Unix(int64(now), 0)
	if current_time.Sub(token_time) > time.Duration(AUTH_WINDOW) {
		log.Error("stale token")
		return errors.New("stale token")
	}

	// TODO: check that their ip address matches
	// PROBLEM: remote_ip is something like: "http://127.0.0.1:8000"
	// instead of "127.0.0.1", so we still need to figure out how
	// to get the IP address out of there (and make sure it is the right
	// end of the connection)

	//	if remote_ip.String() != ip_address {
	//		fmt.Printf("%s %s\n",remote_ip.String(), ip_address)
	//		return uni, errors.New("remote address doesn't match token")
	//	}

	// check that the HMAC matches
	h := hmac.New(sha1.New, []byte(SECRET))
	h.Write([]byte(fmt.Sprintf("%s:%s:%s:%d:%s:%s", uni, sub_prefix, pub_prefix, now, salt, ip_address)))
	sum := fmt.Sprintf("%x", h.Sum(nil))
	if sum != hmc {
		log.WithFields(log.Fields{
			"token":      token,
			"expected":   hmc,
			"calculated": sum,
		}).Error("token HMAC doesn't match")
		return errors.New("token HMAC doesn't match")
	}
	return nil
}

func BuildConnection(ws *websocket.Conn) {
	log.Info("BuildConnection()")
	token := ws.Request().URL.Query().Get("token")
	log.Debug(token)
	var uci userConnectionInfo
	err := validateToken(token, time.Now(), ws.RemoteAddr(), &uci)
	if err != nil {
		log.WithFields(log.Fields{
			"error": err,
		}).Error("validation error")
		// how should this reply to the client?
		return
	}

	onlineUser := &OnlineUser{
		Connection: ws,
		Uci:        uci,
		Send:       make(chan envelope, 256),
	}
	runningRoom.Users[onlineUser] = true
	numClients.Add(1)
	totalClients.Add(1)
	go onlineUser.PushToClient()
	onlineUser.PullFromClient()
	delete(runningRoom.Users, onlineUser)
	numClients.Add(-1)
	log.Info("tore down user connection")
}

// listen on a zmq SUB socket
// and shovel messages from it out to the websockets
func zmqToWebsocket(subsocket zmq.Socket) {
	for {
		address, _ := subsocket.Recv(0)
		content, _ := subsocket.Recv(0)
		log.WithFields(log.Fields{
			"address": string(address),
			"content": string(content),
		}).Info("received a zmq message")
		runningRoom.SendMessage(envelope{string(address), string(content)})
		numMessages.Add(1)
	}
}

// send a message to the zmq PUB socket
func sendMessage(reqsocket zmq.Socket, e envelope) {
	serialized_envelope, _ := json.Marshal(e)
	reqsocket.Send(string(serialized_envelope), 0)
	// wait for a reply
	reqsocket.Recv(0)
}

// take messages from the Incoming channel
// and just shovel them out to the zmq PUB socket
func websocketToZmq(reqsocket zmq.Socket) {
	for msg := range runningRoom.Incoming {
		// this could potentially be done async:
		sendMessage(reqsocket, msg)
	}
}

type ConfigData struct {
	Secret        string
	SubSocket     string
	ReqSocket     string
	WebSocketPort string
	SubKey        string
	Certificate   string
	Key           string
}

type config struct {
	LogLevel string `envconfig:"LOG_LEVEL"`
}

func main() {
	log.SetLevel(log.InfoLevel)
	var configfile string
	flag.StringVar(&configfile, "config", "./windsock.json", "Windsock JSON config file")
	flag.Parse()

	file, err := ioutil.ReadFile(configfile)
	if err != nil {
		log.Fatal(err)
	}

	f := ConfigData{}
	err = json.Unmarshal(file, &f)
	if err != nil {
		log.Fatal(err)
	}
	SECRET = f.Secret

	var c config
	err = envconfig.Process("windsock", &c)
	if err != nil {
		log.Fatal(err.Error())
	}

	// defaults to INFO
	if c.LogLevel == "DEBUG" {
		log.SetLevel(log.DebugLevel)
	}
	if c.LogLevel == "WARN" {
		log.SetLevel(log.WarnLevel)
	}
	if c.LogLevel == "ERROR" {
		log.SetLevel(log.ErrorLevel)
	}
	if c.LogLevel == "FATAL" {
		log.SetLevel(log.FatalLevel)
	}

	subsocket, _ := zmq.NewSocket(zmq.SUB)
	reqsocket, _ := zmq.NewSocket(zmq.REQ)
	defer reqsocket.Close()
	defer subsocket.Close()
	reqsocket.Connect(f.ReqSocket)
	subsocket.SetSubscribe(f.SubKey)
	subsocket.Connect(f.SubSocket)

	InitRoom()

	// two goroutines to move messages in each direction
	go websocketToZmq(*reqsocket)
	go zmqToWebsocket(*subsocket)

	http.Handle("/socket/", websocket.Handler(BuildConnection))

	expvar.Publish("Uptime", expvar.Func(uptime))
	if f.Certificate != "" && f.Key != "" {
		// configured for SSL
		log.Info("starting WSS on ", f.WebSocketPort)
		err = http.ListenAndServeTLS(f.WebSocketPort, f.Certificate, f.Key, nil)
	} else {
		log.Info("starting WS on ", f.WebSocketPort)
		err = http.ListenAndServe(f.WebSocketPort, nil)
	}
	if err != nil {
		log.Fatal("ListenAndServe: " + err.Error())
	}
}
