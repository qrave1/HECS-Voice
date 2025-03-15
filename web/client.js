class VoiceClient {
    constructor() {
        this.roomCode = null;
        this.name = null;
        this.ws = null;
        this.pc = null;
        this.localStream = null;

        // Элементы UI
        this.connectBtn = document.getElementById('connectBtn');
        this.disconnectBtn = document.getElementById('disconnectBtn');
        this.roomCodeInput = document.getElementById('roomCode');
        this.nameInput = document.getElementById('name');
        this.statusDiv = document.getElementById('status');

        this.initializeEventListeners();
    }

    initializeEventListeners() {
        this.connectBtn.addEventListener('click', () => this.connect());
        this.disconnectBtn.addEventListener('click', () => this.disconnect());
    }

    async connect() {
        const code = this.roomCodeInput.value;
        const name = this.nameInput.value;

        if (!/^\d{4}$/.test(code) || !name) {
            alert('Please enter a 4-digit code and your name');
            return;
        }

        try {
            await this.initializeWebSocket(code, name);
            await this.initializeWebRTC();
            this.setUIState(true);
        } catch (err) {
            console.error('Connection error:', err);
            alert('Connection failed: ' + err.message);
        }
    }

    async initializeWebSocket(roomCode, name) {
        this.ws = new WebSocket(`wss://${window.location.host}/ws`);

        return new Promise((resolve, reject) => {
            this.ws.onopen = () => {
                this.ws.send(JSON.stringify({type: 'join', name: name, room: roomCode}));
                resolve();
            };

            this.ws.onerror = (err) => reject(err);

            this.ws.onmessage = (event) => this.handleWSMessage(event);

            this.ws.onclose = () => {
                if (this.pc) this.disconnect();
                alert('Connection closed');
            };
        });
    }

    async initializeWebRTC() {
        this.pc = new RTCPeerConnection({
            iceServers: [{urls: 'stun:stun.l.google.com:19302'}]
        });

        // Настройка обработчиков WebRTC
        this.pc.onicecandidate = (e) => {
            if (e.candidate) {
                console.log('New ICE candidate:', e.candidate);

                this.ws.send(JSON.stringify({
                    type: 'candidate',
                    candidate: e.candidate
                }));
            }
        };

        // Получение аудио с микрофона
        this.localStream = await navigator.mediaDevices.getUserMedia({audio: true});
        this.localStream.getTracks().forEach(track => {
            console.log('Adding local track:', track);

            this.pc.addTrack(track, this.localStream);
        });

        this.pc.addTransceiver('audio', {
            direction: 'sendrecv',
            streams: [this.localStream]
        });

        // Обработка входящего аудио
        this.pc.ontrack = (event) => {
            const audio = new Audio();
            audio.srcObject = event.streams[0];
            audio.play();
        };

        const offer = await this.pc.createOffer();
        await this.pc.setLocalDescription(offer);
        this.ws.send(JSON.stringify({
            type: 'offer',
            sdp: offer.sdp
        }));
    }

    async handleWSMessage(event) {
        const message = JSON.parse(event.data);

        switch (message.type) {
            case 'offer':
                await this.pc.setRemoteDescription(message);
                const answer = await this.pc.createAnswer();
                await this.pc.setLocalDescription(answer);
                this.ws.send(JSON.stringify(answer));
                break;

            case 'answer':
                await this.pc.setRemoteDescription(message);
                break;

            case 'candidate':
                try {
                    await this.pc.addIceCandidate(message.candidate);
                } catch (err) {
                    console.error('Error adding ICE candidate:', err);
                }
                break;
            case 'participants':
                this.updateParticipants(message.list);
                break;

            default:
                console.warn('Unknown message type:', message.type);
        }
    }

    updateParticipants(participants) {
        const list = document.getElementById('participantsList');
        list.innerHTML = participants.map(name => `<li>${name}</li>`).join('');
    }

    disconnect() {
        if (this.pc) {
            this.pc.close();
            this.pc = null;
        }

        if (this.ws) {
            this.ws.close();
            this.ws = null;
        }

        if (this.localStream) {
            this.localStream.getTracks().forEach(track => track.stop());
            this.localStream = null;
        }

        this.setUIState(false);
    }

    setUIState(connected) {
        this.statusDiv.textContent = connected ? 'Connected' : 'Disconnected';
        this.statusDiv.className = `status ${connected ? 'connected' : 'disconnected'}`;
        this.disconnectBtn.style.display = connected ? 'inline-block' : 'none';
        this.connectBtn.style.display = connected ? 'none' : 'inline-block';
    }
}

// Инициализация при загрузке страницы
window.addEventListener('load', () => {
    window.client = new VoiceClient();
});