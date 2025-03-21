package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

var (
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	roomManager = NewRoomManager()
)

type RoomManager struct {
	rooms map[string]*Room
	mu    sync.RWMutex
}

func NewRoomManager() *RoomManager {
	return &RoomManager{
		rooms: make(map[string]*Room),
	}
}

func (rm *RoomManager) GetOrCreate(roomID string) *Room {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	if room, exists := rm.rooms[roomID]; exists {
		return room
	}

	room := NewRoom(roomID)
	rm.rooms[roomID] = room
	return room
}

func (rm *RoomManager) Remove(roomID string) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	delete(rm.rooms, roomID)
}

type Room struct {
	id      string
	clients map[string]*Client
	mu      sync.RWMutex
}

func NewRoom(id string) *Room {
	return &Room{
		id:      id,
		clients: make(map[string]*Client),
	}
}

func (r *Room) AddClient(c *Client) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clients[c.id] = c
}

func (r *Room) RemoveClient(clientID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.clients, clientID)

	if len(r.clients) == 0 {
		roomManager.Remove(r.id)
	}
}

func (r *Room) BroadcastRTP(pkt *rtp.Packet, senderID string) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, client := range r.clients {
		if client.id == senderID {
			continue
		}

		go func(c *Client) {
			c.mu.Lock()
			defer c.mu.Unlock()

			// Обновляем параметры пакета для получателя
			pkt.Header.SequenceNumber = c.sequenceNumber
			pkt.Header.Timestamp = c.timestamp
			pkt.Header.SSRC = c.ssrc

			if err := c.audioTrack.WriteRTP(pkt); err != nil {
				log.Printf("RTP write error: %v", err)
			}

			c.sequenceNumber++
			c.timestamp += 960 // 48kHz * 20ms
		}(client)
	}
}

type Client struct {
	id             string
	name           string
	conn           *websocket.Conn
	pc             *webrtc.PeerConnection
	room           *Room
	audioTrack     *webrtc.TrackLocalStaticRTP
	sequenceNumber uint16
	timestamp      uint32
	ssrc           uint32
	mu             sync.Mutex
}

func createPeerConnection() (*webrtc.PeerConnection, *webrtc.TrackLocalStaticRTP, error) {
	pc, err := webrtc.NewPeerConnection(
		webrtc.Configuration{
			ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}},
		},
	)
	if err != nil {
		return nil, nil, err
	}

	audioTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: "audio/opus"}, "audio", "pion",
	)
	if err != nil {
		return nil, nil, err
	}

	if _, err = pc.AddTrack(audioTrack); err != nil {
		return nil, nil, err
	}

	return pc, audioTrack, nil
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}
	defer conn.Close()

	pc, audioTrack, err := createPeerConnection()
	if err != nil {
		log.Printf("PeerConnection error: %v", err)
		return
	}

	client := &Client{
		id:         uuid.NewString(),
		conn:       conn,
		pc:         pc,
		audioTrack: audioTrack,
		ssrc:       uint32(uuid.New().ID()),
	}

	defer func() {
		if client.room != nil {
			client.room.RemoveClient(client.id)
		}
		pc.Close()
	}()

	pc.OnTrack(
		func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
			if track.Kind() == webrtc.RTPCodecTypeAudio {
				go func() {
					for {
						pkt, _, err := track.ReadRTP()
						if err != nil {
							return
						}
						client.room.BroadcastRTP(pkt, client.id)
					}
				}()
			}
		},
	)

	pc.OnICECandidate(
		func(c *webrtc.ICECandidate) {
			if c == nil {
				return
			}
			conn.WriteJSON(map[string]interface{}{"type": "candidate", "candidate": c.ToJSON()})
		},
	)

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			log.Printf("WebSocket read error: %v", err)
			return
		}
		if err := handleClientMessage(client, msg); err != nil {
			log.Printf("Message handling error: %v", err)
			return
		}
	}
}

func handleClientMessage(c *Client, msg []byte) error {
	var base struct{ Type string }
	if err := json.Unmarshal(msg, &base); err != nil {
		return err
	}

	switch base.Type {
	case "join":
		var data struct{ Name, Room string }
		if err := json.Unmarshal(msg, &data); err != nil {
			return err
		}
		c.name = data.Name
		room := roomManager.GetOrCreate(data.Room)
		room.AddClient(c)
		c.room = room

	case "offer":
		var data struct{ SDP string }
		if err := json.Unmarshal(msg, &data); err != nil {
			return err
		}
		if err := c.pc.SetRemoteDescription(
			webrtc.SessionDescription{
				Type: webrtc.SDPTypeOffer, SDP: data.SDP,
			},
		); err != nil {
			return err
		}
		answer, err := c.pc.CreateAnswer(nil)
		if err != nil {
			return err
		}
		if err = c.pc.SetLocalDescription(answer); err != nil {
			return err
		}
		return c.conn.WriteJSON(map[string]interface{}{"type": "answer", "sdp": answer.SDP})

	case "candidate":
		var data struct{ Candidate webrtc.ICECandidateInit }
		if err := json.Unmarshal(msg, &data); err != nil {
			return err
		}
		return c.pc.AddICECandidate(data.Candidate)

	default:
		return errors.New("unknown message type")
	}
	return nil
}

func main() {
	http.Handle("/", http.FileServer(http.Dir("web")))
	http.HandleFunc("/ws", handleWebSocket)
	log.Println("Server starting on :8000")
	log.Fatal(http.ListenAndServe(":8000", nil))
}
