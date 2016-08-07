package client

import (
    "strconv"
    "encoding/json"
    "unsafe"
    "sync"

    "github.com/streadway/amqp"
    "github.com/porthos-rpc/porthos-go/log"
    "github.com/porthos-rpc/porthos-go/message"
)

type Response struct {
    out chan []byte
    closed bool
    mutex *sync.Mutex
}

// Client is an entry point for making remote calls.
type Client struct {
    serviceName string
    defaultTTL int64
    channel *amqp.Channel
    deliveryChannel <-chan amqp.Delivery
    responseQueue *amqp.Queue
}

func (r *Response) getCorrelationID() string {
    return string(message.UintptrToBytes((uintptr)(unsafe.Pointer(r))))
}

func (r *Response) Out() <-chan []byte {
    return r.out
}

func (r *Response) Dispose() {
    r.mutex.Lock()
    defer r.mutex.Unlock()
    if !r.closed {
        r.closed = true
        close(r.out)
    }
}

// NewBroker creates a new instance of AMQP connection.
func NewBroker(amqpURL string) (*amqp.Connection, error) {
    return amqp.Dial(amqpURL)
}

// NewClient creates a new instance of Client, responsible for making remote calls.
func NewClient(conn *amqp.Connection, serviceName string, defaultTTL int64) (*Client, error) {
    ch, err := conn.Channel()

    if err != nil {
        conn.Close()
        return nil, err
    }

    // create the response queue (let the amqp server to pick a name for us)
    q, err := ch.QueueDeclare("", false, false, true, false, nil)

    if err != nil {
        ch.Close()
        return nil, err
    }

    dc, err := ch.Consume(
            q.Name, // queue
            "",     // consumer
            false,   // auto-ack
            false,  // exclusive
            false,  // no-local
            false,  // no-wait
            nil,    // args
    )

    if err != nil {
        ch.Close()
        return nil, err
    }

    c := &Client{
        serviceName,
        defaultTTL,
        ch,
        dc,
        &q,
    }

    c.start()

    return c, nil
}

func (c *Client) start() {
    go func() {
        for d := range c.deliveryChannel {
            c.processResponse(d)
        }
    }()
}

func (c *Client) processResponse(d amqp.Delivery) {
    d.Ack(false)

    log.Success("Ack. Received response in '%s' for slot: '%d'", d.RoutingKey, []byte(d.CorrelationId))

    address := c.unmarshallCorrelationID(d.CorrelationId)

    res := c.getResponse(address)

    func() {
        res.mutex.Lock()
        defer res.mutex.Unlock()
        if !res.closed {
            res.out <- d.Body
        }
    }()
}

func (c *Client) Call(method string, args ...interface{}) (*Response) {
    body, err := json.Marshal(&message.MessageBody{method, args})

    if err != nil {
        panic(err)
    }

    res := c.makeNewResponse()
    correlationID := res.getCorrelationID()

    err = c.channel.Publish(
        "",             // exchange
        c.serviceName,  // routing key
        false,          // mandatory
        false,          // immediate
        amqp.Publishing{
                Expiration:    strconv.FormatInt(c.defaultTTL, 10),
                ContentType:   "application/json",
                CorrelationId: correlationID,
                ReplyTo:       c.responseQueue.Name,
                Body:          body,
        })

    log.Info("Published method '%s' in '%s'. Expecting response in queue '%s' and slot '%d'", method, c.serviceName, c.responseQueue.Name, []byte(correlationID))

    if err != nil {
        panic(err)
    }

    return res
}

// CallVoid calls a remote service procedure/service which will not provide any return value.
func (c *Client) CallVoid(method string, args ...interface{}) {
    body, err := json.Marshal(&message.MessageBody{method, args})

    if err != nil {
        panic(err)
    }

    err = c.channel.Publish(
        "",             // exchange
        c.serviceName,  // routing key
        false,          // mandatory
        false,          // immediate
        amqp.Publishing{
                ContentType:   "application/json",
                Body:          body,
        })

    if err != nil {
        panic(err)
    }
}

// Close the client and AMQP chanel.
func (c *Client) Close() {
    c.channel.Close()
}

func (c *Client) getResponse(address uintptr) *Response {
    return (*Response)(unsafe.Pointer(uintptr(address)))
}

func (c *Client) makeNewResponse()(*Response){
    return &Response{make(chan []byte), false, new(sync.Mutex)}
}

func (c *Client) unmarshallCorrelationID(correlationID string) (uintptr) {
    return message.BytesToUintptr([]byte(correlationID))
}
