package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"database/sql"

	"context"

	"github.com/go-redis/redis/v8"
	_ "github.com/go-sql-driver/mysql"
	"github.com/gofrs/uuid"
	"github.com/gorilla/pat"
	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
	"github.com/unrolled/render"
	"github.com/urfave/negroni"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

var rd *render.Render

var db *sql.DB
var gormDB *gorm.DB

var client *redis.Client

var ctx = context.Background()

// Redis 클라이언트는 init()함수에서 초기화 합니다. 이렇게 하면 main.go 파일을 실행할 때마다 redis가 연결됩니다.
func init() {
	err := godotenv.Load(".env")
	if err != nil {
		log.Fatal(err)
	}

	//Initializing redis
	dsn := os.Getenv("REDIS_DSN")
	if len(dsn) == 0 {
		dsn = "localhost:6379"
	}
	client = redis.NewClient(&redis.Options{
		Addr:     dsn,
		Password: os.Getenv("REDIS_PASSWORD"),
	})
	_, err = client.Ping(ctx).Result()
	if err != nil {
		panic(err)
	}
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	cors(w)
	fmt.Fprint(w, "Hello World")
}

func cors(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
}

func checkEmail(w http.ResponseWriter, r *http.Request) {
	cors(w)
	email := r.FormValue("email")
	var emailFromDB string
	err := db.QueryRow("select email from UserInfo where email = ?", email).Scan(&emailFromDB)
	if err != nil {
		fmt.Println(err)
		rd.JSON(w, http.StatusBadRequest, "일치하는 이메일이 없습니다.")
	} else {
		rd.JSON(w, http.StatusOK, "ok")
	}
}

type ChatRoom struct {
	Invites   []string `json:"invites"`
	RoomMaker string   `json:"roomMaker"`
	RoomName  string   `json:"roomName"`
	RoomId    string   `json:"roomId"`
}

type Message struct {
	ID          uint      `json:"id"`
	Sender      string    `json:"sender"`
	SenderEmail string    `json:"senderEmail"`
	RoomId      string    `json:"roomId"`
	Msg         string    `json:"msg"`
	Time        time.Time `json:"time"`
}

func makeRoom(w http.ResponseWriter, r *http.Request) {
	cors(w)

	var chatRoom ChatRoom

	json.NewDecoder(r.Body).Decode(&chatRoom)

	if chatRoom.RoomName != "" {
		// 데이터가 비어있지 않다면

		// 방 생성 트랜잭션 시작
		tx, err := db.Begin()
		// 중간에 에러시 롤백
		defer tx.Rollback()

		// roomId로 사용할 uuid
		uuid, err := uuid.NewV4()
		if err != nil {
			panic(err)
		}
		var successMsg []string
		_, err = tx.Exec("insert into ChatRoom (roomMaker, roomName, roomId) values (?, ?, ?)", chatRoom.RoomMaker, chatRoom.RoomName, uuid)
		if err != nil {
			successMsg = append(successMsg, "서버 내부 에러로 방 생성에 실패하였습니다.")
			rd.JSON(w, http.StatusBadRequest, successMsg)
			log.Panic(err)
		}

		// 초대된 인원들마다 UserAndChatRoom에 추가
		// 초대된 인원에 방장도 포함시킴
		chatRoom.Invites = append(chatRoom.Invites, chatRoom.RoomMaker)

		for _, email := range chatRoom.Invites {
			_, err = tx.Exec("insert into UserAndChatRoom values (?, ?)", email, uuid)
			if err != nil {
				successMsg = append(successMsg, "서버 내부 에러로 방 생성에 실패하였습니다.")
				rd.JSON(w, http.StatusBadRequest, successMsg)
				log.Panic(err)
			}
		}
		// 트랜잭션 커밋
		err = tx.Commit()
		if err != nil {
			log.Panic(err)
		}

		successMsg = append(successMsg, "채팅방이 생성되었습니다.", uuid.String())

		rd.JSON(w, http.StatusOK, successMsg)
	}

}

func getRoomList(w http.ResponseWriter, r *http.Request) {
	cors(w)

	email := r.FormValue("email")
	// DB의 UserAndChatroom에서 해당 유저가 속한 채팅방을 모두 불러오기
	var rooms string
	err := db.QueryRow("select GROUP_CONCAT(roomId SEPARATOR '^') from UserAndChatRoom where email = ?", email).Scan(&rooms)
	if err != nil {
		fmt.Println(err)
		rd.JSON(w, http.StatusBadRequest, "방이 없습니다.")
		return
	}
	roomSlice := strings.Split(rooms, "^")
	// 채팅방 roodId를 통해 방 이름, 인원 목록 불러오기

	var chatRooms []ChatRoom
	for _, roomid := range roomSlice {
		var roomName string
		var roomMaker string

		err := db.QueryRow("select roomName, roomMaker from ChatRoom where roomId = ?", roomid).Scan(&roomName, &roomMaker)
		if err != nil {
			fmt.Println(err)
			rd.JSON(w, http.StatusOK, "서버 내부 에러 발생")
			return
		}
		var users string
		err = db.QueryRow("select  GROUP_CONCAT(email SEPARATOR '^')  from UserAndChatRoom where roomId = ?", roomid).Scan(&users)
		if err != nil {
			fmt.Println(err)
			rd.JSON(w, http.StatusOK, "서버 내부 에러 발생")
			return
		}
		userSlice := strings.Split(users, "^")
		chatRoom := ChatRoom{RoomName: roomName, Invites: userSlice, RoomId: roomid, RoomMaker: roomMaker}
		chatRooms = append(chatRooms, chatRoom)
	}
	rd.JSON(w, http.StatusOK, chatRooms)
}

func findClientIndex(email string) int {
	for i, client := range clients {
		if client.email == email {
			return i
		}
	}
	return -1
}

func getMessage(w http.ResponseWriter, r *http.Request) {
	cors(w)

	roomId := r.FormValue("roomId")

	fmt.Println(roomId)
	// 해당 채팅방의 모든 메세지들을 담는 배열.
	var messages []*Message
	// gorm을 통해서 객체 리스트 바로 받아오기
	gormDB.Where("room_id = ?", roomId).Find(&messages)

	rd.JSON(w, http.StatusOK, messages)
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

var clients []*Client

type Client struct {
	conn  *websocket.Conn
	email string
}

func socketHandler(w http.ResponseWriter, r *http.Request) {
	cors(w)

	email := r.URL.Query().Get("email")
	fmt.Println(email)

	conn, err := upgrader.Upgrade(w, r, nil)
	defer conn.Close()

	if err != nil {
		log.Printf("upgrader.Upgrade: %v", err)
		return
	}

	var newClient Client
	newClient.conn = conn
	newClient.email = email
	// clients에 해당 클라이언트가 없으면 그냥 추가, 있으면 교체

	// 있으면 인덱스 알려주고 없으면 -1 반환
	indexOfClient := findClientIndex(email)

	if indexOfClient == -1 {
		// 없으면 그대로 추가
		clients = append(clients, &newClient)
		fmt.Println(len(clients))
	} else {
		// 있으면 원래 있던 거 삭제하고 추가
		clients = append(clients[:indexOfClient], clients[indexOfClient+1:]...)
		clients = append(clients, &newClient)
		fmt.Println(len(clients))
	}

	for {
		messageType, p, err := conn.ReadMessage()
		// 프론트로부터 메세지 객체 수신하여 DB에 저장
		if err != nil {
			log.Printf("conn.ReadMessage: %v", err)
			return
		}
		var message Message
		json.Unmarshal(p, &message)
		// fmt.Println(message.Msg, message.RoomId, message.Sender)
		time := time.Now()
		message.Time = time

		// 데이터베이스에 저장
		// err = db.QueryRow("insert into Message (sender, roomId, msg, time, senderEmail) values (?, ?, ?, ?, ?)", message.Sender, message.RoomId, message.Msg, time, message.SenderEmail).Err()
		gormDB.Create(&Message{Sender: message.Sender, SenderEmail: message.SenderEmail, RoomId: message.RoomId, Msg: message.Msg, Time: time})

		if err != nil {
			log.Printf("%v", err)
		}

		messageToClient, err := json.Marshal(message)
		// 저장이 되었다면 접속된 클라이언트에게 메세지 전송

		for _, client := range clients {
			client.conn.WriteMessage(messageType, messageToClient)
		}

	}
}

func main() {
	// .env 파일에서 환경변수 불러오기 - 시크릿 키 보안을 위함.
	err := godotenv.Load(".env")
	if err != nil {
		log.Fatal(err)
	}

	rd = render.New()
	mux := pat.New()
	n := negroni.Classic()
	n.UseHandler(mux)

	// mysql 연결
	db, err = sql.Open("mysql", os.Getenv("MYSQL"))
	if err != nil {
		fmt.Println(err)
	}
	// 기존에 있던 DB를 gorm과 연결
	gormDB, err = gorm.Open(mysql.New(mysql.Config{
		Conn: db,
	}), &gorm.Config{})
	defer db.Close()

	gormDB.AutoMigrate(&Message{})

	mux.HandleFunc("/check_email", checkEmail)

	mux.HandleFunc("/make_room", makeRoom)
	mux.HandleFunc("/get_room_list", getRoomList)
	mux.HandleFunc("/get_message", getMessage)

	mux.HandleFunc("/ws", socketHandler)

	http.ListenAndServe(":3002", n)
}
