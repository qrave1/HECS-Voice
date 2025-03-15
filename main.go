package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"gopkg.in/hraban/opus.v2"
)

// Глобальные переменные
var (
	rooms    = make(map[string]*Room)
	roomsMu  sync.Mutex
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
)

// Room представляет комнату с клиентами и их аудиотреками
type Room struct {
	clients map[*Client]bool
	tracks  map[string]*webrtc.TrackRemote
	mu      sync.Mutex
}

// Client представляет подключенного клиента
type Client struct {
	id       string
	conn     *websocket.Conn
	pc       *webrtc.PeerConnection
	room     *Room
	audioOut *webrtc.TrackLocalStaticRTP
}

// Создание PeerConnection
func createPeerConnection(clientID string) (*webrtc.PeerConnection, error) {
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
	}
	pc, err := webrtc.NewPeerConnection(config)
	if err != nil {
		return nil, err
	}

	audioTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: "audio/opus"},
		"audio",
		"pion",
	)
	if err != nil {
		return nil, err
	}
	_, err = pc.AddTrack(audioTrack)
	if err != nil {
		return nil, err
	}

	return pc, nil
}

// Генерация уникального ID
func generateClientID() string {
	return uuid.New().String()
}

// Обработка WebSocket
func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("WebSocket upgrade error:", err)
		return
	}
	defer conn.Close()

	clientID := generateClientID()
	pc, err := createPeerConnection(clientID)
	if err != nil {
		log.Println("PeerConnection creation error:", err)
		return
	}
	defer pc.Close()

	client := &Client{
		id:       clientID,
		conn:     conn,
		pc:       pc,
		audioOut: pc.GetSenders()[0].Track().(*webrtc.TrackLocalStaticRTP),
	}

	pc.OnTrack(
		func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
			if track.Kind() == webrtc.RTPCodecTypeAudio {
				client.room.addTrack(clientID, track)
			}
		},
	)

	pc.OnICECandidate(
		func(candidate *webrtc.ICECandidate) {
			if candidate != nil {
				err := conn.WriteJSON(
					map[string]interface{}{
						"type":      "candidate",
						"candidate": candidate.ToJSON().Candidate,
					},
				)
				if err != nil {
					log.Println("ICE candidate send error:", err)
				}
			}
		},
	)

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			log.Println("WebSocket read error:", err)
			removeClient(client)
			return
		}
		handleMessage(client, msg)
	}
}

// Обработка сообщений
func handleMessage(client *Client, msg []byte) {
	var data map[string]interface{}
	err := json.Unmarshal(msg, &data)
	if err != nil {
		log.Println("JSON parse error:", err)
		return
	}

	switch data["type"] {
	case "join":
		roomID := data["room"].(string)
		joinRoom(client, roomID)
	case "offer":
		sdp := data["sdp"].(string)
		offer := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}
		err := client.pc.SetRemoteDescription(offer)
		if err != nil {
			log.Println("SetRemoteDescription error:", err)
			return
		}

		answer, err := client.pc.CreateAnswer(nil)
		if err != nil {
			log.Println("CreateAnswer error:", err)
			return
		}

		err = client.pc.SetLocalDescription(answer)
		if err != nil {
			log.Println("SetLocalDescription error:", err)
			return
		}

		err = client.conn.WriteJSON(
			map[string]interface{}{
				"type": "answer",
				"sdp":  answer.SDP,
			},
		)
		if err != nil {
			log.Println("SDP answer send error:", err)
		}
	case "candidate":
		candidate := data["candidate"].(string)
		err := client.pc.AddICECandidate(webrtc.ICECandidateInit{Candidate: candidate})
		if err != nil {
			log.Println("AddICECandidate error:", err)
		}
	}
}

// Присоединение к комнате
func joinRoom(client *Client, roomID string) {
	roomsMu.Lock()
	room, exists := rooms[roomID]
	if !exists {
		room = &Room{
			clients: make(map[*Client]bool),
			tracks:  make(map[string]*webrtc.TrackRemote),
		}
		rooms[roomID] = room
	}
	roomsMu.Unlock()

	room.mu.Lock()
	room.clients[client] = true
	client.room = room
	room.mu.Unlock()

	if len(room.clients) == 1 {
		go room.mixAudio()
	}
}

// Удаление клиента
func removeClient(client *Client) {
	if client.room != nil {
		client.room.mu.Lock()
		delete(client.room.clients, client)
		delete(client.room.tracks, client.id)
		client.room.mu.Unlock()
	}
}

// Добавление трека
func (r *Room) addTrack(clientID string, track *webrtc.TrackRemote) {
	r.mu.Lock()
	r.tracks[clientID] = track
	r.mu.Unlock()
}

// Микширование аудио
func (r *Room) mixAudio() {
	decoder, err := opus.NewDecoder(48000, 2)
	if err != nil {
		log.Println("Opus decoder init error:", err)
		return
	}
	encoder, err := opus.NewEncoder(48000, 2, opus.AppVoIP)
	if err != nil {
		log.Println("Opus encoder init error:", err)
		return
	}

	for {
		r.mu.Lock()
		if len(r.tracks) == 0 {
			r.mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			continue
		}

		var packets []*rtp.Packet
		for _, track := range r.tracks {
			packet, _, err := track.ReadRTP()
			if err != nil {
				continue
			}
			packets = append(packets, packet)
		}
		r.mu.Unlock()

		if len(packets) == 0 {
			time.Sleep(20 * time.Millisecond)
			continue
		}

		var pcmBuffers [][]float32
		for _, packet := range packets {
			pcm := make([]float32, 4096) // Буфер для декодирования
			n, err := decoder.DecodeFloat32(packet.Payload, pcm)
			if err != nil {
				log.Println("DecodeFloat32 error:", err)
				continue
			}
			pcmBuffers = append(pcmBuffers, pcm[:n])
		}

		if len(pcmBuffers) > 0 {
			// Микширование PCM
			mixedPCM := make([]float32, len(pcmBuffers[0]))
			for _, pcm := range pcmBuffers {
				for i, sample := range pcm {
					mixedPCM[i] += sample
				}
			}

			opusData := make([]byte, 4096) // Достаточно большой буфер

			// Кодирование в Opus
			_, err := encoder.EncodeFloat32(mixedPCM, opusData)
			if err != nil {
				log.Println("Opus encode error:", err)
				continue
			}

			// Создание RTP-пакета
			rtpPacket := &rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					PayloadType:    111,
					SequenceNumber: uint16(time.Now().UnixNano()),
					Timestamp:      uint32(time.Now().UnixNano() / 1000),
					SSRC:           12345,
				},
				Payload: opusData,
			}

			// Отправка клиентам
			r.mu.Lock()
			for client := range r.clients {
				if client.audioOut != nil {
					err := client.audioOut.WriteRTP(rtpPacket)
					if err != nil {
						log.Println("WriteRTP error:", err)
					}
				}
			}
			r.mu.Unlock()
		}

		time.Sleep(20 * time.Millisecond)
	}
}

func main() {
	// Обслуживание статических файлов
	fs := http.FileServer(http.Dir("web"))
	http.Handle("/", fs)
	http.HandleFunc("/ws", handleWebSocket)

	log.Println("Server started on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
