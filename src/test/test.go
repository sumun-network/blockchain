package main

import (
	"fmt"
	"log"
	"math/rand"
	"net"
	"time"
)

func stressLoop(ip string, port string) {
	fmt.Println("Connecting to " + ip + ":" + port)
	conn, err := net.Dial("tcp", ip+":"+port)
	if err != nil {
		log.Fatal("Couldnt connect to validator: "+ip+":"+port, err)
	} else {
		fmt.Fprintf(conn, "CREATE_TX::{\"instruction\":1}\n")
		fmt.Println("CREATE_TX request sent")
	}
}

func test() {
	var i int
	for i = 0; i < 100; i++ {
		if rand.Intn(2) == 0 {
			stressLoop("localhost", "8080")
		} else {
			stressLoop("localhost", "9002")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func main() {
	fmt.Println("Starting network stress test")
	go test()
	for {

	}
}
