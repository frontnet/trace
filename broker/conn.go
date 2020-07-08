package broker

import (
	"encoding/binary"
	"encoding/json"
	"net"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"github.com/unit-io/unitd/message"
	"github.com/unit-io/unitd/message/security"
	lp "github.com/unit-io/unitd/net/lineprotocol"
	"github.com/unit-io/unitd/pkg/log"
	"github.com/unit-io/unitd/pkg/uid"
	"github.com/unit-io/unitd/store"
	"github.com/unit-io/unitd/types"
)

type Conn struct {
	sync.Mutex
	tracked uint32 // Whether the connection was already tracked or not.
	// protocol - NONE (unset), RPC, GRPC, WEBSOCK, CLUSTER
	proto  lp.Proto
	socket net.Conn
	// send     chan []byte
	send               chan lp.Packet
	recv               chan lp.Packet
	pub                chan *lp.Publish
	stop               chan interface{}
	insecure           bool           // The insecure flag provided by client will not perform key validation and permissions check on the topic.
	username           string         // The username provided by the client during connect.
	message.MessageIds                // local identifier of messages
	clientid           uid.ID         // The clientid provided by client during connect or new Id assigned.
	connid             uid.LID        // The locally unique id of the connection.
	service            *Service       // The service for this connection.
	subs               *message.Stats // The subscriptions for this connection.
	// Reference to the cluster node where the connection has originated. Set only for cluster RPC sessions
	clnode *ClusterNode
	// Cluster nodes to inform when disconnected
	nodes map[string]bool

	// Close.
	closeW sync.WaitGroup
	closeC chan struct{}
}

func (s *Service) newConn(t net.Conn, proto lp.Proto) *Conn {
	c := &Conn{
		proto:      proto,
		socket:     t,
		MessageIds: message.NewMessageIds(),
		send:       make(chan lp.Packet, 1), // buffered
		recv:       make(chan lp.Packet),
		pub:        make(chan *lp.Publish),
		stop:       make(chan interface{}, 1), // Buffered by 1 just to make it non-blocking
		connid:     uid.NewLID(),
		service:    s,
		subs:       message.NewStats(),
		// Close
		closeC: make(chan struct{}),
	}

	// Increment the connection counter
	s.meter.Connections.Inc(1)

	Globals.ConnCache.Add(c)
	return c
}

// newRpcConn a new connection in cluster
func (s *Service) newRpcConn(conn interface{}, connid uid.LID, clientid uid.ID) *Conn {
	c := &Conn{
		connid:     connid,
		clientid:   clientid,
		MessageIds: message.NewMessageIds(),
		send:       make(chan lp.Packet, 1), // buffered
		recv:       make(chan lp.Packet),
		pub:        make(chan *lp.Publish),
		stop:       make(chan interface{}, 1), // Buffered by 1 just to make it non-blocking
		service:    s,
		subs:       message.NewStats(),
		clnode:     conn.(*ClusterNode),
		nodes:      make(map[string]bool, 3),
	}

	Globals.ConnCache.Add(c)
	return c
}

// ID returns the unique identifier of the subscriber.
func (c *Conn) ID() string {
	return strconv.FormatUint(uint64(c.connid), 10)
}

// Type returns the type of the subscriber
func (c *Conn) Type() message.SubscriberType {
	return message.SubscriberDirect
}

// Send forwards the message to the underlying client.
func (c *Conn) SendMessage(m *message.Message) bool {
	msg := lp.Publish{
		FixedHeader: lp.FixedHeader{
			Qos: m.Qos,
		},
		MessageID: m.MessageID, // The ID of the message
		Topic:     m.Topic,     // The topic for this message.
		Payload:   m.Payload,   // The payload for this message.
	}

	// Acknowledge the publication
	select {
	case c.pub <- &msg:
	case <-time.After(time.Microsecond * 50):
		return false
	}

	return true
}

// Send forwards raw bytes to the underlying client.
func (c *Conn) SendRawBytes(buf []byte) bool {
	if c == nil {
		return true
	}
	c.closeW.Add(1)
	defer c.closeW.Done()

	select {
	case <-c.closeC:
		return false
	case <-time.After(time.Microsecond * 50):
		return false
	default:
		c.socket.Write(buf)
	}

	return true
}

// Subscribe subscribes to a particular topic.
func (c *Conn) subscribe(msg lp.Subscribe, topic *security.Topic) (err error) {
	c.Lock()
	defer c.Unlock()

	key := string(topic.Key)
	if exists := c.subs.Exist(key); exists && !msg.IsForwarded && Globals.Cluster.isRemoteContract(string(c.clientid.Contract())) {
		// The contract is handled by a remote node. Forward message to it.
		if err := Globals.Cluster.routeToContract(msg, topic, message.SUBSCRIBE, &message.Message{}, c); err != nil {
			log.ErrLogger.Err(err).Str("context", "conn.subscribe").Int64("connid", int64(c.connid)).Msg("unable to subscribe to remote topic")
			return err
		}
		// Add the subscription to Counters
	} else {
		messageId, err := store.Subscription.NewID()
		if err != nil {
			log.ErrLogger.Err(err).Str("context", "conn.subscribe")
		}
		if first := c.subs.Increment(topic.Topic[:topic.Size], key, messageId); first {
			// Subscribe the subscriber
			payload := make([]byte, 5)
			payload[0] = msg.Qos
			binary.LittleEndian.PutUint32(payload[1:5], uint32(c.connid))
			if err = store.Subscription.Put(c.clientid.Contract(), messageId, topic.Topic, payload); err != nil {
				log.ErrLogger.Err(err).Str("context", "conn.subscribe").Str("topic", string(topic.Topic[:topic.Size])).Int64("connid", int64(c.connid)).Msg("unable to subscribe to topic") // Unable to subscribe
				return err
			}
			// Increment the subscription counter
			c.service.meter.Subscriptions.Inc(1)
		}
	}
	return nil
}

// Unsubscribe unsubscribes this client from a particular topic.
func (c *Conn) unsubscribe(msg lp.Unsubscribe, topic *security.Topic) (err error) {
	c.Lock()
	defer c.Unlock()

	key := string(topic.Key)
	// Remove the subscription from stats and if there's no more subscriptions, notify everyone.
	if last, messageId := c.subs.Decrement(topic.Topic[:topic.Size], key); last {
		// Unsubscribe the subscriber
		if err = store.Subscription.Delete(c.clientid.Contract(), messageId, topic.Topic[:topic.Size]); err != nil {
			log.ErrLogger.Err(err).Str("context", "conn.unsubscribe").Str("topic", string(topic.Topic[:topic.Size])).Int64("connid", int64(c.connid)).Msg("unable to unsubscribe to topic") // Unable to subscribe
			return err
		}
		// Decrement the subscription counter
		c.service.meter.Subscriptions.Dec(1)
	}
	if !msg.IsForwarded && Globals.Cluster.isRemoteContract(string(c.clientid.Contract())) {
		// The topic is handled by a remote node. Forward message to it.
		if err := Globals.Cluster.routeToContract(msg, topic, message.UNSUBSCRIBE, &message.Message{}, c); err != nil {
			log.ErrLogger.Err(err).Str("context", "conn.unsubscribe").Int64("connid", int64(c.connid)).Msg("unable to unsubscribe to remote topic")
			return err
		}
	}
	return nil
}

// Publish publishes a message to everyone and returns the number of outgoing bytes written.
func (c *Conn) publish(msg lp.Publish, messageID uint16, topic *security.Topic, payload []byte) (err error) {
	c.service.meter.InMsgs.Inc(1)
	c.service.meter.InBytes.Inc(int64(len(payload)))
	// subscription count
	scount := 0

	conns, err := store.Subscription.Get(c.clientid.Contract(), topic.Topic)
	if err != nil {
		log.ErrLogger.Err(err).Str("context", "conn.publish")
	}
	m := &message.Message{
		MessageID: messageID,
		Topic:     topic.Topic[:topic.Size],
		Payload:   payload,
	}
	for _, connid := range conns {
		m.Qos = connid[0]
		lid := uid.LID(binary.LittleEndian.Uint32(connid[1:5]))
		sub := Globals.ConnCache.Get(lid)
		if sub != nil {
			if m.Qos != 0 && m.MessageID == 0 {
				mID := c.MessageIds.NextID(lp.PUBLISH)
				m.MessageID = c.outboundID(mID)
			}
			if !sub.SendMessage(m) {
				log.ErrLogger.Err(err).Str("context", "conn.publish")
			}
			scount++
		}
	}
	c.service.meter.OutMsgs.Inc(int64(scount))
	c.service.meter.OutBytes.Inc(m.Size() * int64(scount))

	if !msg.IsForwarded && Globals.Cluster.isRemoteContract(string(c.clientid.Contract())) {
		if err = Globals.Cluster.routeToContract(msg, topic, message.PUBLISH, m, c); err != nil {
			log.ErrLogger.Err(err).Str("context", "conn.publish").Int64("connid", int64(c.connid)).Msg("unable to publish to remote topic")
		}
	}
	return err
}

// sendClientID generate unique client and send it to new client
func (c *Conn) sendClientID(clientidentifier string) {
	c.SendMessage(&message.Message{
		Topic:   []byte("unitd/clientid/"),
		Payload: []byte(clientidentifier),
	})
}

// notifyError notifies the connection about an error
func (c *Conn) notifyError(err *types.Error, messageID uint16) {
	err.ID = int(messageID)
	if b, err := json.Marshal(err); err == nil {
		c.SendMessage(&message.Message{
			Topic:   []byte("unitd/error/"),
			Payload: b,
		})
	}
}

func (c *Conn) unsubAll() {
	for _, stat := range c.subs.All() {
		store.Subscription.Delete(c.clientid.Contract(), stat.ID, stat.Topic)
	}
}

func (c *Conn) inboundID(id uint16) message.MID {
	return message.MID(uint32(c.connid) - uint32(id))
}

func (c *Conn) outboundID(mid message.MID) (id uint16) {
	return uint16(uint32(c.connid) - (uint32(mid)))
}

func (c *Conn) storeInbound(m lp.Packet) {
	if c.clientid != nil {
		k := uint64(c.inboundID(m.Info().MessageID))<<32 + uint64(c.clientid.Contract())
		store.Log.PersistInbound(k, m)
	}
}

func (c *Conn) storeOutbound(m lp.Packet) {
	if c.clientid != nil {
		k := uint64(m.Info().MessageID)<<32 + uint64(c.clientid.Contract())
		store.Log.PersistOutbound(k, m)
	}
}

// Close terminates the connection.
func (c *Conn) close() error {
	if r := recover(); r != nil {
		defer log.ErrLogger.Debug().Str("context", "conn.closing").Msgf("panic recovered '%v'", debug.Stack())
	}
	defer c.socket.Close()
	// Signal all goroutines.
	close(c.closeC)
	c.closeW.Wait()
	// Unsubscribe from everything, no need to lock since each Unsubscribe is
	// already locked. Locking the 'Close()' would result in a deadlock.
	// Don't close clustered connection, their servers are not being shut down.
	if c.clnode == nil {
		for _, stat := range c.subs.All() {
			store.Subscription.Delete(c.clientid.Contract(), stat.ID, stat.Topic)
			// Decrement the subscription counter
			c.service.meter.Subscriptions.Dec(1)
		}
	}

	Globals.ConnCache.Delete(c.connid)
	defer log.ConnLogger.Info().Str("context", "conn.close").Int64("connid", int64(c.connid)).Msg("conn closed")
	Globals.Cluster.connGone(c)
	close(c.send)
	// Decrement the connection counter
	c.service.meter.Connections.Dec(1)
	return nil
}
