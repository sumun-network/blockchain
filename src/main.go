package main

import (
	"bufio"
	"crypto"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"os"
	"sort"
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

var pendingBlocks []Block

var startedVoting int64
var newBlockVote = make(map[string]int)
var newBlockVoteData = make(map[string][]Block)

var blockQueue []func()
var handlingABlock bool

var currentBlockIndex int

// candidateBlocks handles incoming blocks for validation
var candidateBlocks = make(chan Block)

// announcements broadcasts winning validator to all nodes
var announcements = make(chan string)

var mutex = &sync.Mutex{}

// validators keeps track of open validators and balances
var validators = make(map[string]int)
var validatorsConn = make(map[int]net.Conn)
var validatorsList = make(map[string]interface{})

var privKey string
var tcpPort string
var stype string
var originAddr string

func ping() string {
	return "pong"
}

func genesis() string {
	return "GENESIS"
}

var instructions = map[int]interface{}{
	0: genesis,
	1: ping,
}

func contains(s []string, str string) bool {
	for _, v := range s {
		if v == str {
			return true
		}
	}

	return false
}

func getConn(addr string, stype string) (net.Conn, error) {
	conn, err := net.Dial("tcp", addr)
	if stype == "origin" && err != nil {
		fmt.Println("INITIAL NODE OFFLINE:", err)
		fmt.Println("Server type is origin, ignoring")
	}
	return conn, err
}

func propagateCommand(text string) {
	known := validatorsList

	fmt.Println("Propagating command "+strings.Split(text, "::")[0]+" to", len(validatorsList), "nodes")

	for _, validator := range known {
		data, _ := validator.(map[string]interface{})
		conn, err := net.Dial("tcp", data["IP"].(string)+":"+data["PORT"].(string))
		if err != nil {
			log.Fatal("Couldnt propagate command to validator: "+data["PUBKEY"].(string)+",", err)
		}
		fmt.Println("Sending to " + data["IP"].(string) + ":" + data["PORT"].(string))
		fmt.Fprintf(conn, text+"\n")
		raw, _ := bufio.NewReader(conn).ReadString('\n')
		message := strings.Split(raw, "\n")[0]
		if message != "OK" {
			fmt.Println(data["IP"].(string) + ":" + data["PORT"].(string) + " responded with string NEQ \"OK\"")
			fmt.Println(message)
		}
	}
}

func main() {
	pu, pk := genKeypair()
	fmt.Println(pu)
	fmt.Println(getPublicFromPrivate(pk))
	fmt.Println(pk)

	startedVoting = 0
	currentBlockIndex = 0
	handlingABlock = false

	privKey = pk

	err := godotenv.Load()
	if err != nil {
		log.Fatal(err)
	}
	tcpPort = os.Getenv("PORT")
	stype = os.Getenv("TYPE")

	originAddr = "localhost:8080"

	c, err := getConn(originAddr, stype)

	if stype != "origin" && err != nil {
		log.Fatal("INITIAL NODE OFFLINE:", err)
	} else {
		if stype != "origin" {
			fmt.Fprintf(c, "REGISTER_VALIDATOR::0.0.0.0::"+tcpPort+"::"+pu+"::"+signMessage(pu, pk)+"\n")

			raw, _ := bufio.NewReader(c).ReadString('\n')
			message := strings.Split(raw, "\n")[0]
			if message == "VALIDATOR_REGISTERED" {
				fmt.Fprintf(c, "GET_PUBKEY\n")
				raw, _ := bufio.NewReader(c).ReadString('\n')
				message := strings.Split(raw, "\n")[0]
				validatorsConn[len(validatorsConn)] = c
				validatorsList[message] = map[string]interface{}{
					"IP":     strings.Split(originAddr, ":")[0],
					"PORT":   strings.Split(originAddr, ":")[1],
					"PUBKEY": message,
				}
				fmt.Fprintf(c, "GET_BLOCKCHAIN\n")
				raw, _ = bufio.NewReader(c).ReadString('\n')
				message = strings.Split(raw, "\n")[0]
				var newbc []Block
				err := json.Unmarshal([]byte(message), &newbc)
				if err != nil {
					log.Println("Error reading blockchain broadcast:", err)
				} else {
					Blockchain = newbc
					//spew.Dump(Blockchain)
				}
				fmt.Println("Validator registered with initial node")
				fmt.Println("Network propagation is in progress this may take up to an hour")
			} else {
				fmt.Println("Error registering validator")
				return
			}
		}

		validators[pu] = getKeyBalance(pu)

		if stype == "origin" {
			// create genesis block
			t := time.Now().Unix()
			genesisBlock := Block{}
			var result map[string]interface{}
			json.Unmarshal([]byte(`{
				"instruction": 0,
				"data": {
					"credits": 600000000000
				}
			}`), &result)
			genesisBlock = Block{0, t, result, calculateBlockHash(genesisBlock), "", ""}
			spew.Dump(genesisBlock)
			Blockchain = append(Blockchain, genesisBlock)
			jd, _ := json.Marshal(Blockchain)
			propagateCommand("PROPAGATE_BLOCKCHAIN::" + string(jd))
		}

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

		go workThroughQueue()

		go voter()

		for {
			conn, err := server.Accept()
			if err != nil {
				log.Fatal(err)
			}
			go handleConn(conn)
		}
	}
}

// pickWinner creates a lottery pool of validators and chooses the validator who gets to forge a block to the blockchain
// by random selecting from the pool, weighted by amount of tokens staked
func pickWinner() {
	mutex.Lock()
	temp := tempBlocks
	mutex.Unlock()

	if len(validators) > 0 {
		lotteryPool := []string{}
		if len(temp) > 0 {
			fmt.Println("Forging a new block with", len(temp), "transactions")
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

				for _, pubkey := range validatorsList {
					setValidators[pubkey.(map[string]interface{})["PUBKEY"].(string)] = validators[pubkey.(map[string]interface{})["PUBKEY"].(string)]
					lotteryPool = append(lotteryPool, pubkey.(map[string]interface{})["PUBKEY"].(string))
				}

				lotteryPool = append(lotteryPool, getPublicFromPrivate(privKey))

				sort.Strings(lotteryPool)
			}

			// randomly pick winner from lottery pool
			s := rand.NewSource(lastBlockTime)
			r := rand.New(s)
			lotteryWinner := lotteryPool[r.Intn(len(lotteryPool))]

			if lotteryWinner == getPublicFromPrivate(privKey) {
				// add block of winner to blockchain and let all the other nodes know
				for _, block := range temp {
					block.Validator = lotteryWinner
					mutex.Lock()
					pendingBlocks = append(pendingBlocks, block)
					mutex.Unlock()
					fmt.Println("Forged new block")
					//fmt.Println("Forged new block, broadcasting blockchain to network")
					//jd, _ := json.Marshal(block)
					//go propagateCommand("PROPAGATE_BLOCKCHAIN::" + string(jd))
					//pubKey := getPublicFromPrivate(privKey)
					//bl, _ := json.Marshal(temp)
					//lotteryAddrs := ""
					//for _, addr := range lotteryPool {
					//	lotteryAddrs += addr + ","
					//}
					//go propagateCommand("PROPAGATE_NEW_BLOCK::" + string(jd) + "::" + signMessage(pubKey, privKey) + "::" + pubKey + "::" + Blockchain[len(Blockchain)-2].Hash + "::" + string(bl) + "::" + lotteryAddrs)
				}
			} else {
				fmt.Println("Block forged by someone else, broadcasting")
				jd, _ := json.Marshal(temp)
				go propagateCommand("PROPAGATE_NEW_BLOCK_UNOWNED::" + string(jd) + "::" + lotteryWinner)
			}

			handlingABlock = false

			mutex.Lock()
			tempBlocks = []Block{}
			mutex.Unlock()
		}
	} else {
		if len(temp) > 0 {
			fmt.Println("Block failed to be forged, no known validators")
		}
	}
}

func verifyMessage(sig string, message string, publicKeyStr string) bool {
	publicKey := base58.Decode(string(base58.Decode(publicKeyStr)))
	return ed25519.Verify(publicKey, []byte(message), base58.Decode(sig))
}

func signMessage(message string, privateKey string) string {
	_, priv, _ := ed25519.GenerateKey(strings.NewReader(string(base58.Decode(string(base58.Decode(privateKey))))))
	signature := ed25519.Sign(priv, []byte(message))
	return base58.Encode(signature)
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

func removeQueueItem(slice []func(), s int) []func() {
	return append(slice[:s], slice[s+1:]...)
}

func workThroughQueue() {
	for {
		if len(blockQueue) > 0 {
			var index int
			for _, block := range blockQueue {
				if !handlingABlock && blockQueue[index] != nil {
					fmt.Println("Handling block", strconv.Itoa(index)+",", len(blockQueue), "blocks left in queue")
					handlingABlock = true
					block()
					blockQueue = blockQueue[1:]
					fmt.Println(blockQueue)
				}
				index++
			}
		}
	}
}

func propagateNewValidator(text string) {
	known := validatorsList

	if !strings.Contains(text, "_PROPAGATE") {
		text = strings.Replace(text, "REGISTER_VALIDATOR", "REGISTER_VALIDATOR_PROPAGATE", -1)
	}
	for _, validator := range known {
		data, _ := validator.(map[string]interface{})
		conn, err := net.Dial("tcp", data["IP"].(string)+":"+data["PORT"].(string))
		if err != nil {
			log.Fatal("Couldnt propagate new validator to validator: "+data["PUBKEY"].(string)+",", err)
		}
		fmt.Println("Sending to " + data["IP"].(string) + ":" + data["PORT"].(string) + ": " + text)
		fmt.Fprintf(conn, text+"\n")
		raw, _ := bufio.NewReader(conn).ReadString('\n')
		message := strings.Split(raw, "\n")[0]
		if message == "VALIDATOR_REGISTERED" {
			fmt.Println("Forwarded validator registration to " + data["IP"].(string) + ":" + data["PORT"].(string) + ": " + message)
		}
	}
}

func voter() {
	for {
		time.Sleep(time.Millisecond * 500)
		if startedVoting != 0 && len(newBlockVote) > 0 && time.Now().Unix()-startedVoting >= 5 {
			fmt.Println("Voting period has ended, applying vote results")
			keys := make([]string, 0, len(newBlockVote))
			for k := range newBlockVote {
				keys = append(keys, k)
			}
			highestVote := 0
			highestKey := ""
			for _, pubkey := range keys {
				if newBlockVote[pubkey] > highestVote {
					highestVote = newBlockVote[pubkey]
					highestKey = pubkey
				}
			}
			fmt.Println("Highest voted:", highestKey, "votes:", highestVote)
			if highestKey != getPublicFromPrivate(privKey) {
				fmt.Println("Requesting blockchain from highest voted validator")
				fmt.Println(validatorsList)
				nbc := newBlockVoteData[highestKey]
				fmt.Println("Applying blockchain to local blockchain")
				Blockchain = append(Blockchain, nbc...)
				fmt.Println("Blockchain applied")
			} else {
				Blockchain = append(Blockchain, pendingBlocks...)
				fmt.Println("Voted for self, ignoring")
			}
			startedVoting = 0
			newBlockVote = make(map[string]int)
			mutex.Lock()
			pendingBlocks = []Block{}
			mutex.Unlock()
		}
		if time.Now().Second()%5 == 0 {
			jd, _ := json.Marshal(pendingBlocks)
			fmt.Println(string(jd))
			pubkey := getPublicFromPrivate(privKey)
			propagateCommand("COMPARE_PENDING_BLOCKCHAIN::" + string(jd) + "::" + signMessage(string(jd), privKey) + "::" + pubkey)
		}
	}
}

func handleConn(conn net.Conn) {
	defer conn.Close()

	var validatorKEYStore string

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
		} else if strings.Split(temp, "::")[0] == "REGISTER_VALIDATOR" || strings.Split(temp, "::")[0] == "REGISTER_VALIDATOR_PROPAGATE" {
			if len(strings.Split(temp, "::")) == 5 {
				validatorREQ := temp
				validatorIP := strings.Split(temp, "::")[1]
				validatorPORT := strings.Split(temp, "::")[2]
				validatorKEY := strings.Split(temp, "::")[3]
				validatorSIGNATURE := strings.Split(temp, "::")[4]

				if !verifyMessage(validatorSIGNATURE, validatorKEY, validatorKEY) {
					io.WriteString(conn, "ERROR::INVALID_SIGNATURE\n")
				} else {
					alreadyExists := false
					for _, pubkey := range validatorsList {
						if pubkey.(map[string]interface{})["PUBKEY"].(string) == validatorKEY {
							alreadyExists = true
							break
						}
					}
					if !alreadyExists && validatorKEY != getPublicFromPrivate(privKey) {
						validatorsList[validatorKEY] = map[string]interface{}{
							"IP":     validatorIP,
							"PORT":   validatorPORT,
							"PUBKEY": validatorKEY,
						}
						validators[validatorKEY] = getKeyBalance(validatorKEY)
						io.WriteString(conn, "VALIDATOR_REGISTERED\n")
						fmt.Println("Registered validator with public key " + validatorKEY)
						validatorKEYStore = validatorKEY
						go propagateNewValidator(string(validatorREQ))
						if strings.Split(temp, "::")[0] == "REGISTER_VALIDATOR_PROPAGATE" {
							fmt.Println("Registering with new validator node")
							conn, err := net.Dial("tcp", validatorIP+":"+validatorPORT)
							if err != nil {
								log.Fatal("Couldnt respond to propagated validator: "+validatorKEY+",", err)
							} else {
								pu := getPublicFromPrivate(privKey)
								fmt.Fprintf(conn, "REGISTER_VALIDATOR::0.0.0.0::"+tcpPort+"::"+pu+"::"+signMessage(pu, privKey)+"\n")
								fmt.Println("Registered with new validator node")
							}
						}
					}
				}
			} else {
				io.WriteString(conn, "ERROR::INVALID_COMMAND_ARGUMENTS\n")
			}
		} else if strings.Split(temp, "::")[0] == "PENDING_BLOCKCHAIN_VOTE" {
			voteWinner := strings.Split(temp, "::")[1]
			voteSignature := strings.Split(temp, "::")[2]
			voteKey := strings.Split(temp, "::")[3]

			if verifyMessage(voteSignature, voteKey, voteKey) {
				alreadyExists := false
				keys := make([]string, 0, len(newBlockVote))
				for k := range newBlockVote {
					keys = append(keys, k)
				}
				for _, pubkey := range keys {
					if pubkey == voteWinner {
						alreadyExists = true
					}
				}
				if voteKey != getPublicFromPrivate(privKey) {
					if startedVoting == 0 {
						startedVoting = time.Now().Unix()
					}
					if !alreadyExists {
						newBlockVote[voteWinner] = 1
						fmt.Println("Voted for " + voteWinner)
						io.WriteString(conn, "OK\n")
					} else {
						newBlockVote[voteWinner] = newBlockVote[voteWinner] + 1
						fmt.Println("Voted for " + voteWinner)
						io.WriteString(conn, "OK\n")
					}
				} else {
					io.WriteString(conn, "ERROR::CANNOT_VOTE_SELF\n")
				}
			} else {
				fmt.Println("Invalid vote from "+voteKey, "(CANNOT VERIFY SIGNATURE)")
				io.WriteString(conn, "ERROR::INVALID_SIGNATURE\n")
			}
		} else if strings.Split(temp, "::")[0] == "COMPARE_PENDING_BLOCKCHAIN" {
			var pendingBc []Block
			validatorBlR := strings.Split(temp, "::")[1]
			validatorSig := strings.Split(temp, "::")[2]
			validatorPub := strings.Split(temp, "::")[3]
			err := json.Unmarshal([]byte(validatorBlR), &pendingBc)
			if err != nil {
				fmt.Println("Failed to process COMPARE_PENDING_BLOCKCHAIN:", err)
				io.WriteString(conn, "ERROR::INVALID_COMMAND_ARGUMENTS\n")
			} else {
				if len(pendingBc) > 0 {
					if pendingBc[0].PrevHash == Blockchain[len(Blockchain)-1].Hash {
						if verifyMessage(validatorSig, validatorBlR, validatorPub) {
							if len(pendingBc) > len(pendingBlocks) {
								fmt.Println("Received new pending blockchain")
								io.WriteString(conn, "OK\n")
								pk := getPublicFromPrivate(privKey)
								alreadyExists := false
								keys := make([]string, 0, len(newBlockVote))
								for k := range newBlockVote {
									keys = append(keys, k)
								}
								for _, pubkey := range keys {
									if pubkey == validatorPub {
										alreadyExists = true
									}
								}
								if validatorPub != getPublicFromPrivate(privKey) {
									if startedVoting == 0 {
										startedVoting = time.Now().Unix()
									}
									if !alreadyExists {
										newBlockVote[validatorPub] = 1
										fmt.Println("Voted for " + validatorPub)
									} else {
										newBlockVote[validatorPub] = newBlockVote[validatorPub] + 1
										fmt.Println("Voted for " + validatorPub)
									}
								}
								newBlockVoteData[validatorPub] = pendingBc
								propagateCommand("PENDING_BLOCKCHAIN_VOTE::" + validatorPub + "::" + signMessage(pk, privKey) + "::" + pk)
							} else if len(pendingBc) == len(pendingBlocks) {
								if pendingBc[len(pendingBc)-1].Timestamp > pendingBlocks[len(pendingBlocks)-1].Timestamp {
									fmt.Println("Received new pending blockchain")
									io.WriteString(conn, "OK\n")
									pk := getPublicFromPrivate(privKey)
									propagateCommand("PENDING_BLOCKCHAIN_VOTE::" + validatorPub + "::" + signMessage(pk, privKey) + "::" + pk)
								} else {
									fmt.Println("Received new pending blockchain, but it is older than the blockchain")
									io.WriteString(conn, "ERROR::CHAIN_OLDER\n")
								}
							} else {
								fmt.Println("Received new pending blockchain, but it is shorter than the blockchain")
								io.WriteString(conn, "ERROR::CHAIN_SHORTER\n")
							}
						} else {
							fmt.Println(validatorPub)
							fmt.Println("Received new pending blockchain, but signature is invalid")
							io.WriteString(conn, "ERROR::INVALID_SIGNATURE\n")
						}
					} else {
						fmt.Println("Received invalid pending blockchain")
						io.WriteString(conn, "ERROR::INVALID_FIRST_HASH\n")
					}
				} else {
					fmt.Println("Cannot use a null-length blockchain")
					io.WriteString(conn, "ERROR::NULL_LENGTH_BLOCKCHAIN\n")
				}
			}
		} else if strings.Split(temp, "::")[0] == "PROPAGATE_NEW_BLOCK_UNOWNED" {
			block := strings.Split(temp, "::")[1]
			pubkey := strings.Split(temp, "::")[2]

			if getPublicFromPrivate(privKey) == pubkey {
				var recvBlock []interface{}
				json.Unmarshal([]byte(block), &recvBlock)

				mutex.Lock()
				oldLastIndex := Blockchain[len(Blockchain)-1]
				mutex.Unlock()

				// create newBlock for consideration to be forged

				for _, block := range recvBlock {
					blockQueue = append(blockQueue, func() {
						// create newBlock for consideration to be forged
						oldLastIndex = Blockchain[len(Blockchain)-1]
						newBlock, err := generateBlock(oldLastIndex, block.(map[string]interface{})["Instruction"].(map[string]interface{}), getPublicFromPrivate(privKey))
						if err != nil {
							log.Println(err)
						}
						if isBlockValid(newBlock, oldLastIndex) {
							candidateBlocks <- newBlock
							fmt.Println("Block added to candidate pool")
							io.WriteString(conn, "OK\n")
						} else {
							fmt.Println("Block is invalid")
							io.WriteString(conn, "ERROR::BLOCK_INVALID\n")
						}
					})
				}
			}
		} else if strings.Split(temp, "::")[0] == "CREATE_TX" {
			go func() {
				instruction := strings.Split(temp, "::")[1]
				pubkey := getPublicFromPrivate(privKey)

				var result map[string]interface{}

				json.Unmarshal([]byte(instruction), &result)

				if len(result) > 0 {
					// if malicious party tries to mutate the chain with a bad input, delete them as a validator and they lose their staked tokens
					insts := make([]string, 0, len(instructions))
					for k := range instructions {
						insts = append(insts, string(k))
					}

					if !contains(insts, string(int64(result["instruction"].(float64)))) {
						log.Printf("validator %v submitted an invalid instruction", pubkey)
						delete(validators, pubkey)
						conn.Close()
					}

					mutex.Lock()
					oldLastIndex := Blockchain[len(Blockchain)-1]
					mutex.Unlock()

					//blockQueue = append(blockQueue, func() {
					// create newBlock for consideration to be forged
					oldLastIndex = Blockchain[len(Blockchain)-1]
					newBlock, err := generateBlock(oldLastIndex, result, pubkey)
					if err != nil {
						log.Println(err)
					}
					if isBlockValid(newBlock, oldLastIndex) {
						candidateBlocks <- newBlock
						io.WriteString(conn, newBlock.Hash+"\n")
					}
					//})
				} else {
					io.WriteString(conn, "ERROR::INVALID_COMMAND_ARGUMENTS\n")
				}
			}()
		} else if strings.Split(temp, "::")[0] == "PROPAGATE_BLOCKCHAIN" {
			var newbc []Block
			err := json.Unmarshal([]byte(strings.Split(temp, "::")[1]), &newbc)
			if err != nil {
				log.Println("Error reading blockchain broadcast:", err)
			} else {
				if len(newbc) > len(Blockchain) {
					lotteryPool := []string{}

					lastBlockTime := newbc[len(newbc)-1].Timestamp

					for _, pubkey := range validatorsList {
						lotteryPool = append(lotteryPool, pubkey.(map[string]interface{})["PUBKEY"].(string))
					}

					lotteryPool = append(lotteryPool, getPublicFromPrivate(privKey))

					sort.Strings(lotteryPool)

					s := rand.NewSource(lastBlockTime)
					r := rand.New(s)
					lotteryWinner := lotteryPool[r.Intn(len(lotteryPool))]

					if getPublicFromPrivate(privKey) == lotteryWinner {
						Blockchain = newbc
						//spew.Dump(Blockchain)
					} else {
						fmt.Println("PROPAGATE_BLOCKCHAIN command is invalid")
						propagateCommand("SUSPEND_VALIDATOR_VOTE::" + validatorKEYStore)
					}
				} else {
					log.Println("Received blockchain is not longer than current blockchain")
				}
			}
		} else if strings.Split(temp, "::")[0] == "PROPAGATE_NEW_BLOCK" {
			var newb map[string]interface{}
			var newbc []Block
			err := json.Unmarshal([]byte(strings.Split(temp, "::")[1]), &newb)
			err = json.Unmarshal([]byte(strings.Split(temp, "::")[len(strings.Split(temp, "::"))-2]), &newbc)
			vSig := strings.Split(temp, "::")[2]
			vKey := strings.Split(temp, "::")[3]
			prevBlockHash := strings.Split(temp, "::")[4]
			if err != nil {
				log.Println("Error reading blockchain broadcast:", err)
				io.WriteString(conn, "ERROR::INVALID_BLOCK\n")
			} else {
				if verifyMessage(vSig, vKey, vKey) {
					currentBlockIndex++
					block := Block{
						Index:       int(newb["Index"].(float64)),
						Timestamp:   int64(newb["Timestamp"].(float64)),
						Instruction: newb["Instruction"].(map[string]interface{}),
						Hash:        newb["Hash"].(string),
						PrevHash:    newb["PrevHash"].(string),
						Validator:   newb["Validator"].(string),
					}
					lotteryPool := []string{}

					lastBlockTime := newbc[len(newbc)-1].Timestamp

					for _, pubkey := range validatorsList {
						lotteryPool = append(lotteryPool, pubkey.(map[string]interface{})["PUBKEY"].(string))
					}

					for _, block := range Blockchain {
						if prevBlockHash == block.Hash {
							fmt.Println("Referenced block is valid")
						}
					}

					//for _, addr := range lotteryPool {
					//	if strings.Contains(lotteryData, addr) {
					//		lotteryPool = strings.Split(lotteryData, ",")
					//	}
					//}

					lotteryPool = append(lotteryPool, getPublicFromPrivate(privKey))

					sort.Strings(lotteryPool)

					s := rand.NewSource(lastBlockTime)
					r := rand.New(s)
					lotteryWinner := lotteryPool[r.Intn(len(lotteryPool))]

					if vKey == lotteryWinner {
						if isBlockValid(block, Blockchain[len(Blockchain)-1]) {
							for _, block := range newbc {
								var found bool
								found = false
								for {
									for _, b := range Blockchain {
										if b.Hash == block.PrevHash {
											fmt.Println("Previous block is found")
											Blockchain = append(Blockchain, block)
											found = true
										}
									}
									if found {
										break
									}
								}
								//spew.Dump(Blockchain)
							}
							fmt.Println("PROPAGATE_NEW_BLOCK command is valid")
							io.WriteString(conn, "SUCCESS::BLOCK_ADDED\n")
						} else {
							fmt.Println("PROPAGATE_NEW_BLOCK command is invalid")
							io.WriteString(conn, "ERROR::BLOCK_INVALID\n")
						}
					} else {
						fmt.Println("PROPAGATE_NEW_BLOCK command by " + vKey + " is invalid")
						io.WriteString(conn, "ERROR::BLOCK_INVALID\n")
						fmt.Println("Refreshing blockchain from initial block")
						time.Sleep(time.Second * 5)
						c, err := getConn(originAddr, stype)
						if err != nil {
							log.Println("Error connecting to origin server:", err)
						} else {
							fmt.Fprintf(c, "GET_BLOCKCHAIN\n")
							raw, _ := bufio.NewReader(c).ReadString('\n')
							message := strings.Split(raw, "\n")[0]
							var newbc []Block
							err := json.Unmarshal([]byte(message), &newbc)
							if err != nil {
								log.Println("Error reading blockchain broadcast:", err)
							} else {
								Blockchain = newbc
								//spew.Dump(Blockchain)
							}
						}
					}
				}
			}
		} else if strings.Split(temp, "::")[0] == "SUSPEND_VALIDATOR_VOTE" {
			validatorAddress := strings.Split(temp, "::")[1]
			fmt.Println("Recv validator suspend vote:", validatorAddress)
		} else if strings.Split(temp, "::")[0] == "GET_BLOCKCHAIN" {
			bc, _ := json.Marshal(Blockchain)
			io.WriteString(conn, string(bc)+"\n")
		} else if strings.Split(temp, "::")[0] == "GET_PENDING_BLOCKCHAIN" {
			bc, _ := json.Marshal(pendingBlocks)
			io.WriteString(conn, string(bc)+"\n")
		} else if strings.Split(temp, "::")[0] == "GET_PUBKEY" {
			io.WriteString(conn, getPublicFromPrivate(privKey)+"\n")
		} else {
			io.WriteString(conn, "ERROR::INVALID_REQUEST\n")
		}
	}

	//go func() {
	//	for {
	//		msg := <-announcements
	//		io.WriteString(conn, msg)
	//	}
	//}()
	// validator address
	var address string

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
}

// isBlockValid makes sure block is valid by checking index
// and comparing the hash of the previous block
func isBlockValid(newBlock, oldBlock Block) bool {
	if oldBlock.Index+1 != newBlock.Index {
		fmt.Println(oldBlock.Index, newBlock.Index)
		if oldBlock.Index == newBlock.Index {
			newBlock.Index = newBlock.Index + 1
		} else if oldBlock.Index+1 == newBlock.Index-1 {
			newBlock.Index = newBlock.Index - 1
		} else {
			return false
		}
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
	record := string(block.Index) + strconv.Itoa(int(block.Timestamp)) + string(fmt.Sprint(block.Instruction)) + block.PrevHash
	return calculateHash(record)
}

// generateBlock creates a new block using previous block's hash
func generateBlock(oldBlock Block, Instruction map[string]interface{}, address string) (Block, error) {

	var newBlock Block

	t := time.Now().UTC().UnixNano() / 1e6

	newBlock.Index = oldBlock.Index + 1
	newBlock.Timestamp = t
	newBlock.Instruction = Instruction
	newBlock.PrevHash = oldBlock.Hash
	newBlock.Hash = calculateBlockHash(newBlock)
	newBlock.Validator = address

	return newBlock, nil
}
