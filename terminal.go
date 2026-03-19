package terminal

import (
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		// origem validada pelo middleware JWT antes de chegar aqui
		return true
	},
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
}

// Handler inicia uma sessão de terminal via WebSocket.
// O cliente envia keystrokes como texto e recebe output como binário.
func Handler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("terminal: upgrade falhou: %v", err)
		return
	}
	defer conn.Close()

	// shell restrito — pode ser trocado por rbash ou uma allowlist de comandos
	shell := os.Getenv("SOFIAOS_SHELL")
	if shell == "" {
		shell = "/bin/bash"
	}

	cmd := exec.Command(shell)
	cmd.Env = append(os.Environ(),
		"TERM=xterm-256color",
		"SOFIAOS_SESSION=1",
	)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		log.Printf("terminal: stdin pipe: %v", err)
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("terminal: stdout pipe: %v", err)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		log.Printf("terminal: stderr pipe: %v", err)
		return
	}

	if err := cmd.Start(); err != nil {
		log.Printf("terminal: start: %v", err)
		conn.WriteMessage(websocket.TextMessage, []byte("\r\n[sofiaos] erro ao iniciar shell\r\n"))
		return
	}
	defer cmd.Process.Kill()

	done := make(chan struct{})

	// stdout → WS
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				conn.WriteMessage(websocket.BinaryMessage, buf[:n])
			}
			if err != nil {
				break
			}
		}
		close(done)
	}()

	// stderr → WS
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stderr.Read(buf)
			if n > 0 {
				conn.WriteMessage(websocket.BinaryMessage, buf[:n])
			}
			if err != nil {
				break
			}
		}
	}()

	// WS → stdin
	go func() {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				cmd.Process.Kill()
				return
			}
			if _, err := io.Writer(stdin).Write(msg); err != nil {
				return
			}
		}
	}()

	<-done
}
