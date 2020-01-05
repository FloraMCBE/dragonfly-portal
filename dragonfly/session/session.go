package session

import (
	"bytes"
	"fmt"
	"github.com/dragonfly-tech/dragonfly/dragonfly/player/chat"
	"github.com/dragonfly-tech/dragonfly/dragonfly/world"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
	"github.com/sandertv/gophertunnel/minecraft/text"
	"github.com/sirupsen/logrus"
	"sync"
	"sync/atomic"
	"time"
)

// Session handles incoming packets from connections and sends outgoing packets by providing a thin layer
// of abstraction over direct packets. A Session basically 'controls' an entity.
type Session struct {
	log *logrus.Logger

	c     Controllable
	world *world.World
	conn  *minecraft.Conn

	cmdOrigin     protocol.CommandOrigin
	scoreboardObj atomic.Value

	chunkBuf       *bytes.Buffer
	chunkLoader    atomic.Value
	chunkRadius    int32
	maxChunkRadius int32

	// currentEntityRuntimeID holds the runtime ID assigned to the last entity. It is incremented for every
	// entity spawned to the session.
	currentEntityRuntimeID uint64
	entityMutex            sync.RWMutex
	// entityRuntimeIDs holds a list of all runtime IDs of entities spawned to the session.
	entityRuntimeIDs map[world.Entity]uint64

	// onStop is called when the session is stopped. The controllable passed is the controllable that the
	// session controls.
	onStop func(controllable Controllable)
}

// Nop represents a no-operation session. It does not do anything when sending a packet to it.
var Nop = &Session{}

// session is a slice of all open sessions. It is protected by the sessionMutex, which must be locked whenever
// accessing the value.
var sessions []*Session
var sessionMutex sync.Mutex

// selfEntityRuntimeID is the entity runtime (or unique) ID of the controllable that the session holds.
const selfEntityRuntimeID = 1

// New returns a new session using a controllable entity. The session will control this entity using the
// packets that it receives.
// New takes the connection from which to accept packets. It will start handling these packets after a call to
// Session.Start().
func New(c Controllable, conn *minecraft.Conn, w *world.World, maxChunkRadius int, log *logrus.Logger) *Session {
	s := &Session{
		c:              c,
		conn:           conn,
		log:            log,
		chunkBuf:       bytes.NewBuffer(make([]byte, 0, 4096)),
		world:          w,
		chunkRadius:    int32(maxChunkRadius / 2),
		maxChunkRadius: int32(maxChunkRadius),
		entityRuntimeIDs: map[world.Entity]uint64{
			// We initialise the runtime ID of the controllable of the session. It will always have runtime ID
			// 1, because we treat entity runtime IDs as session-local.
			c: selfEntityRuntimeID,
		},
		currentEntityRuntimeID: 1,
	}
	s.chunkLoader.Store(world.NewLoader(maxChunkRadius/2, w, s))
	s.scoreboardObj.Store("")
	return s
}

// Start makes the session start handling incoming packets from the client and initialises the controllable of
// the session in the world.
// The function passed will be called when the session stops running.
func (s *Session) Start(onStop func(controllable Controllable)) {
	s.onStop = onStop
	s.initPlayerList()

	s.world.AddEntity(s.c)
	s.SendAvailableCommands()

	go s.handlePackets()

	yellow := text.Yellow()
	chat.Global.Println(yellow(s.conn.IdentityData().DisplayName, "has joined the game"))
}

// Close closes the session, which in turn closes the controllable and the connection that the session
// manages.
func (s *Session) Close() error {
	_ = s.c.Close()
	_ = s.conn.Close()
	_ = s.chunkLoader.Load().(*world.Loader).Close()
	s.world.RemoveEntity(s.c)

	yellow := text.Yellow()
	chat.Global.Println(yellow(s.conn.IdentityData().DisplayName, "has left the game"))

	// This should always be called last due to the timing of the removal of entity runtime IDs.
	s.closePlayerList()

	s.entityMutex.Lock()
	s.entityRuntimeIDs = map[world.Entity]uint64{}
	s.entityMutex.Unlock()

	if s.onStop != nil {
		s.onStop(s.c)
		s.onStop = nil
	}
	return nil
}

// RequestClose requests the session to close. It will close as soon as the next call to read is made.
func (s *Session) RequestClose() {
	_ = s.conn.Close()
}

// handlePackets continuously handles incoming packets from the connection. It processes them accordingly.
// Once the connection is closed, handlePackets will return.
func (s *Session) handlePackets() {
	c := make(chan struct{})
	defer func() {
		c <- struct{}{}
		_ = s.Close()
	}()
	go s.sendChunks(c)
	for {
		pk, err := s.conn.ReadPacket()
		if err != nil {
			return
		}
		if err := s.handlePacket(pk); err != nil {
			// An error occurred during the handling of a packet. Print the error and stop handling any more
			// packets.
			s.log.Errorf("error processing packet from %v: %v\n", s.conn.RemoteAddr(), err)
			return
		}
	}
}

// sendChunks continuously sends chunks to the player, until a value is sent to the closeChan passed.
func (s *Session) sendChunks(closeChan <-chan struct{}) {
	t := time.NewTicker(time.Second / 20)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if err := s.chunkLoader.Load().(*world.Loader).Load(4); err != nil {
				// The world was closed. We need to close the session as soon as possible.

				s.log.Errorf("error loading chunk: %v", err)
				continue
			}
		case <-closeChan:
			return
		}
	}
}

// handlePacket handles an incoming packet, processing it accordingly. If the packet had invalid data or was
// otherwise not valid in its context, an error is returned.
func (s *Session) handlePacket(pk packet.Packet) error {
	switch pk := pk.(type) {
	case *packet.Text:
		return s.handleText(pk)
	case *packet.CommandRequest:
		return s.handleCommandRequest(pk)
	case *packet.MovePlayer:
		return s.handleMovePlayer(pk)
	case *packet.RequestChunkRadius:
		return s.handleRequestChunkRadius(pk)
	case *packet.BossEvent: // No need to do anything here. We don't care about these when they're incoming.
	default:
		s.log.Debugf("unhandled packet %T%v from %v\n", pk, fmt.Sprintf("%+v", pk)[1:], s.conn.RemoteAddr())
	}
	return nil
}

// writePacket writes a packet to the session's connection if it is not Nop.
func (s *Session) writePacket(pk packet.Packet) {
	if s == Nop {
		return
	}
	_ = s.conn.WritePacket(pk)
}

// initPlayerList initialises the player list of the session and sends the session itself to all other
// sessions currently open.
func (s *Session) initPlayerList() {
	sessionMutex.Lock()
	sessions = append(sessions, s)
	for _, session := range sessions {
		// Add the player of the session to all sessions currently open, and add the players of all sessions
		// currently open to the player list of the new session.
		session.addToPlayerList(s)
		s.addToPlayerList(session)
	}
	sessionMutex.Unlock()
}

// closePlayerList closes the player list of the session and removes the session from the player list of all
// other sessions.
func (s *Session) closePlayerList() {
	sessionMutex.Lock()
	n := make([]*Session, 0, len(sessions)-1)
	for _, session := range sessions {
		if session != s {
			n = append(n, session)
		}
		// Remove the player of the session from the player list of all other sessions.
		session.removeFromPlayerList(s)
	}
	sessions = n
	sessionMutex.Unlock()
}