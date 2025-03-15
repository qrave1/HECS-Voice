package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"golang.org/x/sync/errgroup"
	"gopkg.in/hraban/opus.v2"
)

var (
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true }, // В продакшене замените на конкретные домены
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
	go room.Start()
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
	tracks  map[string]*webrtc.TrackRemote
	mu      sync.RWMutex
	g       errgroup.Group

	audioOut     *webrtc.TrackLocalStaticRTP
	opusEncoder  *opus.Encoder
	opusDecoder  *opus.Decoder
	closeCh      chan struct{}
	mixingBuffer []float32
}

func NewRoom(id string) *Room {
	encoder, _ := opus.NewEncoder(48000, 2, opus.AppVoIP)
	decoder, _ := opus.NewDecoder(48000, 2)

	return &Room{
		id:           id,
		clients:      make(map[string]*Client),
		tracks:       make(map[string]*webrtc.TrackRemote),
		opusEncoder:  encoder,
		opusDecoder:  decoder,
		closeCh:      make(chan struct{}),
		mixingBuffer: make([]float32, 960*2), // 20ms * 48kHz * 2 channels
	}
}

func (r *Room) Start() {
	r.g.Go(r.mixAudio)
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
	delete(r.tracks, clientID)

	if len(r.clients) == 0 {
		close(r.closeCh)
		roomManager.Remove(r.id)
	}
}

func (r *Room) AddTrack(clientID string, track *webrtc.TrackRemote) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tracks[clientID] = track
}

func (r *Room) broadcastParticipants() {
	r.mu.RLock()
	defer r.mu.RUnlock()

	log.Println(r.clients)

	// Собираем список имен участников
	participants := make([]string, 0, len(r.clients))
	for _, client := range r.clients {
		participants = append(participants, client.name)
	}

	// Отправляем список каждому клиенту
	for _, client := range r.clients {
		err := client.conn.WriteJSON(
			map[string]interface{}{
				"type": "participants",
				"list": participants,
			},
		)
		if err != nil {
			log.Println("Failed to send participants list:", err)
		}
	}
}

func joinRoom(client *Client, roomID string, name string) {
	room := roomManager.GetOrCreate(roomID)

	room.AddClient(client)

	// Добавляем клиента в комнату
	room.mu.Lock()
	client.room = room
	room.mu.Unlock()

	log.Println("Client", client.id, "joined room:", roomID)

	// Отправляем клиенту список участников
	room.broadcastParticipants()
}

type Client struct {
	id        string
	name      string
	conn      *websocket.Conn
	pc        *webrtc.PeerConnection
	room      *Room
	audioSend *webrtc.TrackLocalStaticRTP
}

func createPeerConnection() (*webrtc.PeerConnection, *webrtc.TrackLocalStaticRTP, error) {
	pc, err := webrtc.NewPeerConnection(
		webrtc.Configuration{
			ICEServers: []webrtc.ICEServer{
				{URLs: []string{"stun:stun.l.google.com:19302"}},
			},
		},
	)
	if err != nil {
		return nil, nil, err
	}

	audioTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: "audio/opus"},
		"audio",
		"mix",
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
		id:        uuid.NewString(),
		conn:      conn,
		pc:        pc,
		audioSend: audioTrack,
	}

	defer func() {
		if client.room != nil {
			client.room.RemoveClient(client.id)
		}
		pc.Close()
	}()

	pc.OnTrack(
		func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
			if track.Kind() == webrtc.RTPCodecTypeAudio {
				client.room.AddTrack(client.id, track)
			}
		},
	)

	pc.OnICECandidate(
		func(c *webrtc.ICECandidate) {
			if c == nil {
				return
			}

			if err := conn.WriteJSON(
				map[string]interface{}{
					"type":      "candidate",
					"candidate": c.ToJSON(),
				},
			); err != nil {
				log.Printf("ICE candidate error: %v", err)
			}
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
	var base struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(msg, &base); err != nil {
		return err
	}

	log.Printf("new event: %s", string(msg))

	switch base.Type {
	case "join":
		var data struct {
			Name string `json:"name"`
			Room string `json:"room"`
		}
		if err := json.Unmarshal(msg, &data); err != nil {
			return err
		}

		c.name = data.Name

		joinRoom(c, data.Room, data.Name)
	case "disconnect":

	case "offer":
		var data struct {
			SDP string `json:"sdp"`
		}
		if err := json.Unmarshal(msg, &data); err != nil {
			return err
		}

		if err := c.pc.SetRemoteDescription(
			webrtc.SessionDescription{
				Type: webrtc.SDPTypeOffer,
				SDP:  data.SDP,
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

		return c.conn.WriteJSON(
			map[string]interface{}{
				"type": "answer",
				"sdp":  answer.SDP,
			},
		)

	case "candidate":
		var data struct {
			Candidate webrtc.ICECandidateInit `json:"candidate"`
		}
		if err := json.Unmarshal(msg, &data); err != nil {
			return err
		}

		if err := c.pc.AddICECandidate(data.Candidate); err != nil {
			return err
		}

	default:
		return errors.New("unknown message type")
	}

	return nil
}

func (r *Room) mixAudio() error {
	defer log.Printf("Room %s mixing stopped", r.id)

	var (
		sequenceNumber uint16
		timestamp      uint32
	)

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-r.closeCh:
			return nil
		case <-ticker.C:
			r.mu.RLock()
			tracks := make([]*webrtc.TrackRemote, 0, len(r.tracks))
			for _, t := range r.tracks {
				tracks = append(tracks, t)
			}
			r.mu.RUnlock()

			if len(tracks) == 0 {
				continue
			}

			mixed := r.mixingBuffer[:0]

			for _, track := range tracks {
				pkt, _, err := track.ReadRTP()
				if err != nil {
					continue
				}

				pcm := make([]float32, 960*2) // 20ms * stereo
				_, err = r.opusDecoder.DecodeFloat32(pkt.Payload, pcm)
				if err != nil {
					log.Println("opus decoder error:", err)

					continue
				}

				if len(mixed) == 0 {
					mixed = append(mixed, pcm...)
				} else {
					for i := range pcm {
						if i < len(mixed) {
							mixed[i] += pcm[i]
						}
					}
				}
			}

			if len(mixed) == 0 {
				continue
			}

			for i := range mixed {
				mixed[i] /= float32(len(tracks))
			}

			opusData := make([]byte, 1000)
			n, err := r.opusEncoder.EncodeFloat32(mixed, opusData)
			if err != nil {
				continue
			}

			sequenceNumber++
			timestamp += 960 // 48kHz * 0.02s

			pkt := &rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					PayloadType:    111,
					SequenceNumber: sequenceNumber,
					Timestamp:      timestamp,
					SSRC:           12345,
				},
				Payload: opusData[:n],
			}

			r.mu.Lock()
			for _, client := range r.clients {
				// todo здесь добавить проверку чтобы не отправлять треки самому себе
				if err := client.audioSend.WriteRTP(pkt); err != nil {
					log.Printf("RTP write error: %v", err)
				}
			}
			r.mu.Unlock()
		}
	}
}

func main() {
	http.Handle("/", http.FileServer(http.Dir("web")))
	http.HandleFunc("/ws", handleWebSocket)

	log.Println("Server starting on :8000")
	err := http.ListenAndServe(":8000", nil)
	if err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
