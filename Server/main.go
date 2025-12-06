/*
Version: 0.01
Author: Mang Zhang, Shenzhen China
Release Date: 2025-12-06
Project Name: GoPercyFRP
Description: A tool to remote control your internal Windows or Linux PC.
Copy Rights: MIT License
Email: m13692277450@outlook.com
Mobile: +86-13692277450
HomePage: www.pavogroup.top , github.com/13692277450
*/
package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/base64"
	"flag"
	"fmt"
	"image/png"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chzyer/readline"
)

const (
	DefaultServerPort = "2006"
)

var (
	serverPort = flag.String("port", DefaultServerPort, "Server port")
	exitChan   = make(chan struct{})
)

func main() {
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s [-port PORT]\n", os.Args[0])
		fmt.Println("Example: gofrpserver.exe -port 2006")
		fmt.Println("         gofrpserver.exe")
		flag.PrintDefaults()
	}

	flag.Parse()

	log.Println("Starting server")
	listener, err := net.Listen("tcp", ":"+*serverPort)
	if err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}

	log.Printf("Server started, listening on port %s", *serverPort)

	// Handle graceful shutdown
	go func() {
		<-exitChan
		log.Println("Shutting down server...")
		listener.Close()
		os.Exit(0)
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			// Check if we're shutting down
			select {
			case <-exitChan:
				return
			default:
				log.Printf("Failed to accept connection: %v", err)
				continue
			}
		}

		log.Printf("New connection from: %s", conn.RemoteAddr())
		go handleClient(conn)
	}
}

func handleClient(conn net.Conn) {
	defer conn.Close()

	// Channels for communication between goroutines
	commandChan := make(chan string, 10)
	shutdownChan := make(chan struct{})
	var once sync.Once

	// Start a goroutine to read client responses
	go readClientResponse(conn, shutdownChan)

	// Start a goroutine to read commands from stdin with readline support
	go readCommandsFromStdin(commandChan)

	// Main loop to send commands to client
	for {
		select {
		case command := <-commandChan:
			if command == "" {
				continue
			}

			// Exit command
			if command == "exit" {
				log.Println("Exit command received, shutting down server...")
				// Send exit command to client
				conn.Write([]byte("exit\n"))
				// Close shutdown channel safely
				once.Do(func() {
					close(shutdownChan)
				})
				// Signal server to exit
				close(exitChan)
				return
			}

			// Send command to client
			_, err := conn.Write([]byte(command + "\n"))
			if err != nil {
				log.Printf("Failed to send command: %v", err)
				once.Do(func() {
					close(shutdownChan)
				})
				return
			}

			log.Printf("Command sent: %s", command)
		case <-shutdownChan:
			log.Println("Client handler shutting down")
			return
		}
	}
}

func readCommandsFromStdin(commandChan chan<- string) {
	// Create readline instance with history support
	rl, err := readline.NewEx(&readline.Config{
		Prompt:          "Please enter command (cmd <command> or ps <command>): ",
		HistoryFile:     "/tmp/gofrp_history",
		AutoComplete:    completer,
		InterruptPrompt: "^C",
		EOFPrompt:       "exit",
	})
	if err != nil {
		log.Printf("Failed to initialize readline: %v", err)
		// Fallback to simple input
		readCommandsSimple(commandChan)
		return
	}
	defer rl.Close()

	for {
		line, err := rl.Readline()
		if err != nil {
			if err == readline.ErrInterrupt {
				continue
			} else if err == io.EOF {
				// Send exit command when EOF
				select {
				case commandChan <- "exit":
				default:
				}
				return
			}
			log.Printf("Failed to read command from stdin: %v", err)
			continue
		}

		command := strings.TrimSpace(line)
		if command == "" {
			continue
		}

		// Special case: help command
		if command == "help" {
			fmt.Println(`Help:
Input "cmd dir d:\test" to execute a CMD command
Input "cmd capture screen" to take current picture and send back, which will be saved to current folder as image
Input "send d:\test\test.txt" to request client to send a file back
Input "ps <command>" to execute a PowerShell command
Input "help" to show this help message
Input "exit" to terminate the server and client`)
			rl.SetPrompt("Please enter command (cmd <command> or ps <command>): ")
			continue
		}

		// Add to history
		rl.SaveHistory(command)

		select {
		case commandChan <- command:
		default:
			log.Println("Warning: Command channel is full, dropping command")
		}

		// Update prompt for next iteration
		rl.SetPrompt("Please enter command (cmd <command> or ps <command>): ")
	}
}

// Simple autocomplete for command prefixes
var completer = readline.NewPrefixCompleter(
	readline.PcItem("cmd"),
	readline.PcItem("ps"),
	readline.PcItem("send"),
	readline.PcItem("help"),
	readline.PcItem("exit"),
)

// Fallback function for simple input when readline setup fails
func readCommandsSimple(commandChan chan<- string) {
	stdinReader := bufio.NewReader(os.Stdin)

	for {
		fmt.Print("Please enter command (cmd <command> or ps <command>): ")
		command, err := stdinReader.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				log.Printf("Failed to read command from stdin: %v", err)
			}
			// Send exit command when EOF
			select {
			case commandChan <- "exit":
			default:
			}
			return
		}

		command = strings.TrimSpace(command)
		if command == "" {
			continue
		}

		// Special case: help command
		if command == "help" {
			fmt.Println(`Help:
Input "cmd dir d:\test" to execute a CMD command
Input "cmd capture screen" to take current picture and send back, which will be saved to current folder as image
Input "send d:\test\test.txt" to request client to send a file back
Input "ps <command>" to execute a PowerShell command
Input "help" to show this help message
Input "exit" to terminate the server and client`)
			continue
		}

		select {
		case commandChan <- command:
		default:
			log.Println("Warning: Command channel is full, dropping command")
		}
	}
}
func readClientResponse(conn net.Conn, shutdownChan chan struct{}) {
	defer func() {
		// Recover from potential panic when closing already closed channels
		if r := recover(); r != nil {
			log.Printf("Recovered from panic in readClientResponse: %v", r)
		}
	}()

	reader := bufio.NewReader(conn)
	var isReceivingScreenshot bool
	var screenshotData strings.Builder
	var expectedSize int

	var isReceivingFile bool
	var fileData bytes.Buffer // 使用bytes.Buffer替代strings.Builder处理二进制数据
	var expectedFileSize int64
	var fileName string
	//var receivedChunks int64
	var totalBytes int64
	var lastProgress int
	var startTime time.Time
	var chunkCount int
	var errorCount int

	for {
		// Check if we should shutdown
		select {
		case <-shutdownChan:
			log.Println("Client response reader shutting down")
			return
		default:
		}

		// 设置更长的超时时间，避免大数据传输时超时
		conn.SetReadDeadline(time.Now().Add(30 * time.Second))

		// Read client response
		response, err := reader.ReadString('\n')
		if err != nil {
			// Check if it's a timeout (used for shutdown checking)
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				if isReceivingFile {
					fmt.Printf("\r--- Waiting for data... (timeout) ---")
				}
				continue
			}

			if err != io.EOF {
				log.Printf("Failed to read client response: %v", err)
			}
			return
		}

		// Reset read deadline
		conn.SetReadDeadline(time.Time{})

		response = strings.TrimSpace(response)

		// Handle file transfer data reception
		if isReceivingFile {
			if response == "FILE_TRANSFER_END" {
				// Save the file
				elapsed := time.Since(startTime)
				speed := float64(totalBytes) / elapsed.Seconds() / 1024 // KB/s
				fmt.Printf("\n--- File transfer completed in %.2f seconds (%.2f KB/s) ---\n",
					elapsed.Seconds(), speed)
				fmt.Printf("--- Received %d chunks, %d errors ---\n", chunkCount, errorCount)

				saveFile(&fileData, fileName, expectedFileSize, totalBytes)
				isReceivingFile = false
				fileData.Reset()
				//receivedChunks = 0
				totalBytes = 0
				chunkCount = 0
				errorCount = 0
				lastProgress = 0
				continue
			}

			// 跳过空行
			if response == "" {
				continue
			}

			chunkCount++

			// 解析chunk头信息
			if strings.HasPrefix(response, "CHUNK:") {
				// 找到第三个冒号，分离header和data
				firstColon := strings.Index(response, ":")
				secondColon := strings.Index(response[firstColon+1:], ":") + firstColon + 1
				thirdColon := strings.Index(response[secondColon+1:], ":") + secondColon + 1

				if firstColon >= 0 && secondColon > firstColon && thirdColon > secondColon {
					// 提取base64数据部分
					base64Data := response[thirdColon+1:]

					// 解码前清理数据
					base64Data = strings.TrimSpace(base64Data)
					base64Data = strings.ReplaceAll(base64Data, "\r", "")
					base64Data = strings.ReplaceAll(base64Data, "\n", "")

					// 确保base64数据长度是4的倍数
					if len(base64Data)%4 != 0 {
						padding := 4 - len(base64Data)%4
						base64Data += strings.Repeat("=", padding)
					}

					// 解码base64数据
					decoded, err := base64.StdEncoding.DecodeString(base64Data)
					if err != nil {
						// 尝试不同的解码方法
						decoded, err = base64.RawStdEncoding.DecodeString(base64Data)
						if err != nil {
							errorCount++
							log.Printf("Warning: Failed to decode chunk %d (len=%d): %v",
								chunkCount, len(base64Data), err)
							log.Printf("Base64 preview: %.100s...", base64Data)
							continue
						}
					}

					// 写入到缓冲区
					n, err := fileData.Write(decoded)
					if err != nil {
						errorCount++
						log.Printf("Warning: Failed to write chunk %d: %v", chunkCount, err)
						continue
					}

					totalBytes += int64(n)

					// Calculate and display progress
					if expectedFileSize > 0 {
						progress := int(float64(totalBytes) / float64(expectedFileSize) * 100)
						if progress > lastProgress || chunkCount%100 == 0 || progress == 100 {
							fmt.Printf("\r--- Receiving: %s [%3d%%] %d/%d bytes (chunks: %d, errors: %d) ---",
								fileName, progress, totalBytes, expectedFileSize, chunkCount, errorCount)
							lastProgress = progress
						}
					}
				} else {
					errorCount++
					log.Printf("Warning: Malformed chunk header: %.100s...", response)
				}
			} else {
				// 处理旧格式的chunk（无header）
				base64Data := strings.TrimSpace(response)
				if base64Data != "" {
					// 确保base64数据长度是4的倍数
					if len(base64Data)%4 != 0 {
						padding := 4 - len(base64Data)%4
						base64Data += strings.Repeat("=", padding)
					}

					decoded, err := base64.StdEncoding.DecodeString(base64Data)
					if err != nil {
						errorCount++
						log.Printf("Warning: Failed to decode legacy chunk %d: %v", chunkCount, err)
						continue
					}

					n, err := fileData.Write(decoded)
					if err != nil {
						errorCount++
						log.Printf("Warning: Failed to write legacy chunk %d: %v", chunkCount, err)
						continue
					}

					totalBytes += int64(n)

					// Calculate and display progress
					if expectedFileSize > 0 {
						progress := int(float64(totalBytes) / float64(expectedFileSize) * 100)
						if progress > lastProgress || chunkCount%100 == 0 || progress == 100 {
							fmt.Printf("\r--- Receiving: %s [%3d%%] %d/%d bytes (chunks: %d, errors: %d) ---",
								fileName, progress, totalBytes, expectedFileSize, chunkCount, errorCount)
							lastProgress = progress
						}
					}
				}
			}
			continue
		}

		// Handle screenshot data reception
		if isReceivingScreenshot {
			if response == "SCREENSHOT_END" {
				// Save the screenshot
				saveScreenshot(screenshotData.String(), expectedSize)
				isReceivingScreenshot = false
				screenshotData.Reset()
				continue
			}

			// Accumulate screenshot data
			screenshotData.WriteString(response)
			continue
		}

		// Check for file transfer start marker
		if strings.HasPrefix(response, "FILE_TRANSFER_START:") {
			parts := strings.SplitN(response, ":", 3)
			if len(parts) == 3 {
				name := parts[1]
				size, err := strconv.ParseInt(parts[2], 10, 64)
				if err == nil {
					isReceivingFile = true
					fileName = name
					expectedFileSize = size
					fileData.Reset()
					//receivedChunks = 0
					totalBytes = 0
					chunkCount = 0
					errorCount = 0
					lastProgress = 0
					startTime = time.Now()
					fmt.Printf("\n--- Receiving file: %s (size: %d bytes) ---\n", name, size)
					fmt.Printf("Progress: [  0%%] 0/%d bytes", size)
					continue
				}
			}
		}

		// Check for screenshot start marker
		if strings.HasPrefix(response, "SCREENSHOT_START:") {
			parts := strings.Split(response, ":")
			if len(parts) == 2 {
				size, err := strconv.Atoi(parts[1])
				if err == nil {
					isReceivingScreenshot = true
					expectedSize = size
					screenshotData.Reset()
					fmt.Println("\n--- Receiving screenshot ---")
					continue
				}
			}
		}

		// Check for end marker
		if response == "---END---" {
			fmt.Println("\n--- Command execution completed ---")
			fmt.Print("Please enter command (cmd <command> or ps <command>): ")
			continue
		}

		// Output client response
		fmt.Println(response)
	}
}

func saveFile(fileData *bytes.Buffer, fileName string, expectedSize int64, actualSize int64) {
	// 获取缓冲区中的数据
	decoded := fileData.Bytes()

	// Validate size
	if int64(len(decoded)) != expectedSize {
		diff := expectedSize - int64(len(decoded))
		if diff > 0 {
			log.Printf("Warning: File incomplete. Expected %d bytes, got %d bytes (missing: %d bytes)",
				expectedSize, len(decoded), diff)
		} else {
			log.Printf("Warning: File larger than expected. Expected %d bytes, got %d bytes (extra: %d bytes)",
				expectedSize, len(decoded), -diff)
		}
	}

	// 避免覆盖现有文件
	originalName := fileName
	counter := 1
	for {
		if _, err := os.Stat(fileName); os.IsNotExist(err) {
			break
		}
		ext := filepath.Ext(originalName)
		name := strings.TrimSuffix(originalName, ext)
		fileName = fmt.Sprintf("%s_%d%s", name, counter, ext)
		counter++
	}

	// Create file
	file, err := os.Create(fileName)
	if err != nil {
		log.Printf("Failed to create file: %v", err)
		return
	}
	defer file.Close()

	// Write data to file
	n, err := file.Write(decoded)
	if err != nil {
		log.Printf("Failed to write file: %v", err)
		return
	}

	// 验证文件完整性（对于ZIP文件）
	if strings.HasSuffix(strings.ToLower(fileName), ".zip") {
		// 尝试打开ZIP文件验证
		reader, err := zip.NewReader(bytes.NewReader(decoded), int64(len(decoded)))
		if err != nil {
			fmt.Printf("\nWarning: ZIP file may be corrupted: %v\n", err)
		} else {
			fmt.Printf("\nZIP file verified: %d files\n", len(reader.File))
		}
	}

	fmt.Printf("\n--- File saved as %s (%d bytes) ---\n", fileName, n)
	fmt.Print("Please enter command (cmd <command> or ps <command>): ")
}

// Add padding to base64 string if needed
func addPadding(data string) string {
	padding := len(data) % 4
	if padding > 0 {
		data += strings.Repeat("=", 4-padding)
	}
	return data
}

func saveScreenshot(data string, expectedSize int) {
	// Clean the data by removing any whitespace that might have been added
	data = strings.TrimSpace(data)

	// Decode base64 data
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		log.Printf("Failed to decode screenshot data: %v", err)
		log.Printf("Data length: %d, First 100 chars: %.100s", len(data), data)
		// Try to decode with padding if needed
		data = addPadding(data)
		decoded, err = base64.StdEncoding.DecodeString(data)
		if err != nil {
			log.Printf("Failed to decode screenshot data even with padding: %v", err)
			return
		}
	}

	// Validate size
	if len(decoded) != expectedSize {
		log.Printf("Warning: Expected %d bytes, got %d bytes", expectedSize, len(decoded))
	}

	// Create filename with timestamp
	filename := fmt.Sprintf("screenshot_%s.png", time.Now().Format("20060102_150405"))

	// Create file
	file, err := os.Create(filename)
	if err != nil {
		log.Printf("Failed to create screenshot file: %v", err)
		return
	}
	defer file.Close()

	// Decode and validate PNG
	_, err = png.Decode(bytes.NewReader(decoded))
	if err != nil {
		log.Printf("Failed to decode PNG data: %v", err)
		return
	}

	// Write raw data to file
	_, err = file.Write(decoded)
	if err != nil {
		log.Printf("Failed to write screenshot to file: %v", err)
		return
	}

	fmt.Printf("\n--- Screenshot saved as %s ---\n", filename)
	fmt.Print("Please enter command (cmd <command> or ps <command>): ")
}
