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
	"strconv"
	"strings"
	"time"

	"github.com/chzyer/readline"
)

const (
	DefaultServerPort = "2006"
)

var (
	serverPort = flag.String("port", DefaultServerPort, "Server port")
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
	defer listener.Close()

	log.Printf("Server started, listening on port %s", *serverPort)

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("Failed to accept connection: %v", err)
			continue
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

			// Send command to client
			_, err := conn.Write([]byte(command + "\n"))
			if err != nil {
				log.Printf("Failed to send command: %v", err)
				close(shutdownChan)
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
				close(commandChan)
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
Input "ps <command>" to execute a PowerShell command
Input "help" to show this help message`)
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
	readline.PcItem("help"),
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
			close(commandChan)
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
Input "ps <command>" to execute a PowerShell command
Input "help" to show this help message`)
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
	defer close(shutdownChan)

	reader := bufio.NewReader(conn)
	var isReceivingScreenshot bool
	var screenshotData strings.Builder
	var expectedSize int

	for {
		// Read client response
		response, err := reader.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				log.Printf("Failed to read client response: %v", err)
			}
			return
		}

		response = strings.TrimSpace(response)

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

func saveScreenshot(data string, expectedSize int) {
	// Decode base64 data
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		log.Printf("Failed to decode screenshot data: %v", err)
		return
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
