package main

import (
	"bufio"
	"crypto"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcutil/base58"
	"github.com/davecgh/go-spew/spew"
	"github.com/joho/godotenv"
)

// Block represents each 'item' in the blockchain
type Block struct {
	Index       int
	Timestamp   int64
	Instruction map[string]interface{}
	Hash        string
	PrevHash    string
	Validator   string
}

// Blockchain is a series of validated Blocks
var Blockchain []Block
var tempBlocks []Block

// candidateBlocks handles incoming blocks for validation
var candidateBlocks = make(chan Block)

// announcements broadcasts winning validator to all nodes
var announcements = make(chan string)

var mutex = &sync.Mutex{}

// validators keeps track of open validators and balances
var validators = make(map[string]int)
var validatorsList = make(map[string]interface{})

func ping() string {
	return "pong"
}

var instructions = map[int]interface{}{
	0: ping,
}

func contains(s []string, str string) bool {
	for _, v := range s {
		if v == str {
			return true
		}
	}

	return false
}

func main() {
	pu, pk := genKeypair("apapi")
	fmt.Println(pu)
	fmt.Println(getPublicFromPrivate(pk))
	fmt.Println(pk)

	err := godotenv.Load()
	if err != nil {
		log.Fatal(err)
	}

	// create genesis block
	t := time.Now().Unix()
	genesisBlock := Block{}
	var result map[string]interface{}
	json.Unmarshal([]byte("{}"), &result)
	genesisBlock = Block{0, t, result, calculateBlockHash(genesisBlock), "", ""}
	spew.Dump(genesisBlock)
	Blockchain = append(Blockchain, genesisBlock)

	tcpPort := os.Getenv("PORT")

	// start TCP and serve TCP server
	server, err := net.Listen("tcp", ":"+tcpPort)
	if err != nil {
		log.Fatal(err)
	}
	log.Println("TCP Server Listening on port", tcpPort)
	defer server.Close()

	go func() {
		for candidate := range candidateBlocks {
			mutex.Lock()
			tempBlocks = append(tempBlocks, candidate)
			mutex.Unlock()
		}
	}()

	go func() {
		for {
			pickWinner()
		}
	}()

	for {
		conn, err := server.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go handleConn(conn)
	}
}

// pickWinner creates a lottery pool of validators and chooses the validator who gets to forge a block to the blockchain
// by random selecting from the pool, weighted by amount of tokens staked
func pickWinner() {
	time.Sleep(5 * time.Second)
	mutex.Lock()
	temp := tempBlocks
	mutex.Unlock()

	lotteryPool := []string{}
	if len(temp) > 0 {

		lastBlockTime := temp[len(temp)-1].Timestamp

		// slightly modified traditional proof of stake algorithm
		// from all validators who submitted a block, weight them by the number of staked tokens
		// in traditional proof of stake, validators can participate without submitting a block to be forged
	OUTER:
		for _, block := range temp {
			// if already in lottery pool, skip
			for _, node := range lotteryPool {
				if block.Validator == node {
					continue OUTER
				}
			}

			// lock list of validators to prevent data race
			mutex.Lock()
			setValidators := validators
			mutex.Unlock()

			k, ok := setValidators[block.Validator]
			if ok {
				for i := 0; i < k; i++ {
					lotteryPool = append(lotteryPool, block.Validator)
				}
			}
		}

		// randomly pick winner from lottery pool
		s := rand.NewSource(lastBlockTime)
		r := rand.New(s)
		lotteryWinner := lotteryPool[r.Intn(len(lotteryPool))]

		// add block of winner to blockchain and let all the other nodes know
		for _, block := range temp {
			if block.Validator == lotteryWinner {
				mutex.Lock()
				Blockchain = append(Blockchain, block)
				mutex.Unlock()
				for _ = range validators {
					announcements <- "\nwinning validator: " + lotteryWinner + "\n"
				}
				break
			}
		}
	}

	mutex.Lock()
	tempBlocks = []Block{}
	mutex.Unlock()
}

func getPublicFromPrivate(privateKey string) string {
	pubKey, _, _ := ed25519.GenerateKey(strings.NewReader(string(base58.Decode(string(base58.Decode(privateKey))))))
	return base58.Encode([]byte(base58.Encode(pubKey)))
}

func RandStringBytesRmndr(n int) string {
	const letterBytes = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	b := make([]byte, n)
	for i := range b {
		b[i] = letterBytes[rand.Int63()%int64(len(letterBytes))]
	}
	return string(b)
}

func genKeypair(seed ...string) (string, string) {
	t := time.Now().String() + RandStringBytesRmndr(1024)

	if seed != nil {
		t = seed[0]
	}

	p := calculateHash(t)

	s := strings.NewReader(p)

	pubKey, _, _ := ed25519.GenerateKey(s)

	encodedpub := base58.Encode([]byte(base58.Encode(pubKey)))
	encodedpriv := base58.Encode([]byte(base58.Encode([]byte(p))))

	return encodedpub, encodedpriv
}

func getKeyBalance(key string) int {
	// TODO: Scan blockchain to get the key's balance
	return 1
}

func handleConn(conn net.Conn) {
	defer conn.Close()

	for {
		netData, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			fmt.Println(err)
			return
		}

		temp := strings.TrimSpace(string(netData))
		if strings.Split(temp, "::")[0] == "GET_KNOWN_VALIDATORS" {
			netData, err := json.Marshal(validatorsList)
			if err != nil {
				fmt.Println(err)
				return
			}
			io.WriteString(conn, string(netData)+"\n")
		} else if strings.Split(temp, "::")[0] == "REGISTER_VALIDATOR" {
			if len(strings.Split(temp, "::")) == 6 {
				validatorIP := strings.Split(temp, "::")[1]
				validatorPORT := strings.Split(temp, "::")[2]
				validatorKEY := strings.Split(temp, "::")[3]
				validatorSIGNATURE := strings.Split(temp, "::")[4]
				validatorPUBKEY := strings.Split(temp, "::")[5]

				fmt.Println(validatorPUBKEY)

				key, err := x509.ParsePKIXPublicKey([]byte(validatorPUBKEY))

				if err != nil {
					io.WriteString(conn, "ERROR::"+err.Error()+"\n")
				} else {
					pubKey := key.(*rsa.PublicKey)

					signature, err := base64.StdEncoding.DecodeString(validatorSIGNATURE)

					if err != nil {
						io.WriteString(conn, "ERROR::INVALID_SIGNATURE\n")
					} else {
						hash := calculateHashBin(validatorKEY)
						err := rsa.VerifyPKCS1v15(pubKey, crypto.SHA256, hash, signature)

						if err != nil {
							io.WriteString(conn, "ERROR::INVALID_SIGNATURE\n")
						} else {
							avalidators := make([]string, 0, len(validators))
							for k := range validators {
								avalidators = append(avalidators, string(k))
							}

							if contains(avalidators, validatorKEY) {
								io.WriteString(conn, "VALIDATOR_ALREADY_REGISTERED\n")
								continue
							} else {
								validatorsList[validatorKEY] = map[string]interface{}{
									"IP":     validatorIP,
									"PORT":   validatorPORT,
									"PUBKEY": validatorKEY,
								}
								validators[validatorKEY] = getKeyBalance(validatorKEY)
								io.WriteString(conn, "VALIDATOR_REGISTERED\n")
							}
						}
					}
				}
			} else {
				io.WriteString(conn, "ERROR::INVALID_COMMAND_ARGUMENTS\n")
			}
		} else {
			io.WriteString(conn, "ERROR::INVALID_REQUEST\n")
		}
	}

	go func() {
		for {
			msg := <-announcements
			io.WriteString(conn, msg)
		}
	}()
	// validator address
	var address string

	// allow user to allocate number of tokens to stake
	// the greater the number of tokens, the greater chance to forging a new block
	io.WriteString(conn, "Enter token balance:")
	scanBalance := bufio.NewScanner(conn)
	for scanBalance.Scan() {
		balance, err := strconv.Atoi(scanBalance.Text())
		if err != nil {
			log.Printf("%v not a number: %v", scanBalance.Text(), err)
			return
		}
		address, _ = genKeypair()
		io.WriteString(conn, "Validator address: "+address)
		validators[address] = balance
		fmt.Println(validators)
		break
	}

	io.WriteString(conn, "\nEnter a new instruction:")

	scanBPM := bufio.NewScanner(conn)

	go func() {
		for {
			// take in BPM from stdin and add it to blockchain after conducting necessary validation
			for scanBPM.Scan() {
				var result map[string]interface{}
				instruction := scanBPM.Text()
				json.Unmarshal([]byte(instruction), &result)
				// if malicious party tries to mutate the chain with a bad input, delete them as a validator and they lose their staked tokens
				insts := make([]string, 0, len(instructions))
				for k := range instructions {
					insts = append(insts, string(k))
				}

				if !contains(insts, string(int64(result["instruction"].(float64)))) {
					log.Printf("validator %v submitted an invalid instruction", address)
					delete(validators, address)
					conn.Close()
					break
				}

				mutex.Lock()
				oldLastIndex := Blockchain[len(Blockchain)-1]
				mutex.Unlock()

				// create newBlock for consideration to be forged
				newBlock, err := generateBlock(oldLastIndex, result, address)
				if err != nil {
					log.Println(err)
					continue
				}
				if isBlockValid(newBlock, oldLastIndex) {
					candidateBlocks <- newBlock
				}
				io.WriteString(conn, "\nEnter a new instruction:")
			}
			break
		}
	}()

	// simulate receiving broadcast
	for {
		time.Sleep(30 * time.Second)
		mutex.Lock()
		output, err := json.Marshal(Blockchain)
		mutex.Unlock()
		if err != nil {
			log.Fatal(err)
		}
		//fmt.Println(output)
		io.WriteString(conn, string(output)+"\n")
	}

}

// isBlockValid makes sure block is valid by checking index
// and comparing the hash of the previous block
func isBlockValid(newBlock, oldBlock Block) bool {
	if oldBlock.Index+1 != newBlock.Index {
		return false
	}

	if oldBlock.Hash != newBlock.PrevHash {
		return false
	}

	if calculateBlockHash(newBlock) != newBlock.Hash {
		return false
	}

	return true
}

// SHA256 hasing
// calculateHash is a simple SHA256 hashing function
func calculateHash(s string) string {
	h := crypto.SHA224.New()
	h.Write([]byte(s))
	hashed := h.Sum(nil)
	return hex.EncodeToString(hashed)
}

func calculateHashBin(s string) []byte {
	h := crypto.SHA224.New()
	h.Write([]byte(s))
	hashed := h.Sum(nil)
	return hashed
}

//calculateBlockHash returns the hash of all block information
func calculateBlockHash(block Block) string {
	record := string(block.Index) + string(block.Timestamp) + string(fmt.Sprint(block.Instruction)) + block.PrevHash
	return calculateHash(record)
}

// generateBlock creates a new block using previous block's hash
func generateBlock(oldBlock Block, Instruction map[string]interface{}, address string) (Block, error) {

	var newBlock Block

	t := time.Now().Unix()

	newBlock.Index = oldBlock.Index + 1
	newBlock.Timestamp = t
	newBlock.Instruction = Instruction
	newBlock.PrevHash = oldBlock.Hash
	newBlock.Hash = calculateBlockHash(newBlock)
	newBlock.Validator = address

	return newBlock, nil
}
