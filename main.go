package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

var appName string
var connectionString string
var databaseConn *pgx.Conn
var clients = make(map[chan string]bool)
var broadcast = make(chan string)

type Todo struct {
	ID          uint32     `json:"id"`
	Title       *string    `json:"title" binding:"required"`
	Description *string    `json:"description"`
	Due_Date    *time.Time `json:"due_date"`
	Status      *string    `json:"status"`
	Created_At  *time.Time `json:"created_at"`
}

func Connect(connectionString string) (*pgx.Conn, error) {
	conn, err := pgx.Connect(context.Background(), connectionString)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func getHealth(c *gin.Context) {

	databaseRes := "disconnected"
	if databaseConn != nil {
		"database": 
		databaseRes = "connected"
	}

	c.JSON(200, gin.H{
		"status":   "ok",
		"app":      appName,
		"database": databaseRes,
	})
}

func getTodos(c *gin.Context) {
	status := c.Query("status")

	var rows pgx.Rows
	var err error

	if status != "" {
		rows, err = databaseConn.Query(
			context.Background(),
			"SELECT * FROM todos WHERE status = $1",
			status,
		)
	} else {
		rows, err = databaseConn.Query(
			context.Background(),
			"SELECT * FROM todos",
		)
	}

	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()

	var todos []Todo
	for rows.Next() {
		var todo Todo
		err := rows.Scan(
			&todo.ID,
			&todo.Title,
			&todo.Description,
			&todo.Due_Date,
			&todo.Status,
			&todo.Created_At,
		)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		todos = append(todos, todo)
	}

	c.JSON(200, todos)
}

func createTodo(c *gin.Context) {
	var todo Todo

	if err := c.ShouldBindJSON(&todo); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	var id uint32
	err := databaseConn.QueryRow(
		context.Background(),
		`INSERT INTO todos (title, description, due_date, status)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id`,
		todo.Title,
		todo.Description,
		todo.Due_Date,
		todo.Status,
	).Scan(&id)

	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}

	todo.ID = id
	c.JSON(201, todo)
}

func editTodo(c *gin.Context) {
	idParam := c.Param("id")

	id, err := strconv.Atoi(idParam)
	if err != nil {
		c.JSON(400, gin.H{"error": "invalid id"})
		return
	}

	var todo Todo
	if err := c.ShouldBindJSON(&todo); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	cmdTag, err := databaseConn.Exec(
		context.Background(),
		`UPDATE todos
		 SET title = COALESCE($1, title),
		     description = COALESCE($2, description),
		     due_date = COALESCE($3, due_date),
		     status = COALESCE($4, status)
		 WHERE id = $5`,
		todo.Title,
		todo.Description,
		todo.Due_Date,
		todo.Status,
		id,
	)

	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}

	if cmdTag.RowsAffected() == 0 {
		c.JSON(404, gin.H{"error": "todo not found"})
		return
	}

	c.JSON(200, gin.H{"message": "todo updated"})
}

func deleteTodo(c *gin.Context) {
	idParam := c.Param("id")

	id, err := strconv.Atoi(idParam)
	if err != nil {
		c.JSON(400, gin.H{"error": "invalid id"})
		return
	}

	cmdTag, err := databaseConn.Exec(
		context.Background(),
		"DELETE FROM todos WHERE id = $1",
		id,
	)

	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}

	if cmdTag.RowsAffected() == 0 {
		c.JSON(404, gin.H{"error": "todo not found"})
		return
	}

	c.Status(204)
}

func getOverdueTodos(c *gin.Context) {
	rows, err := databaseConn.Query(
		context.Background(),
		"SELECT * FROM todos WHERE due_date < NOW() AND status != 'done'",
	)

	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()

	var todos []Todo
	for rows.Next() {
		var todo Todo
		err := rows.Scan(
			&todo.ID,
			&todo.Title,
			&todo.Description,
			&todo.Due_Date,
			&todo.Status,
			&todo.Created_At,
		)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		todos = append(todos, todo)
	}

	c.JSON(200, todos)
}

func alerts(c *gin.Context) {
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(500, gin.H{"error": "streaming unsupported"})
		return
	}

	clientChan := make(chan string)

	clients[clientChan] = true

	defer func() {
		delete(clients, clientChan)
		close(clientChan)
	}()

	pingTicker := time.NewTicker(30 * time.Second)
	defer pingTicker.Stop()

	clientGone := c.Request.Context().Done()

	for {
		select {

		case <-clientGone:
			return

		case msg := <-clientChan:
			fmt.Fprintf(c.Writer, "event: todo_alert\n")
			fmt.Fprintf(c.Writer, "data: %s\n\n", msg)
			flusher.Flush()

		case <-pingTicker.C:
			fmt.Fprintf(c.Writer, "event: ping\n")
			fmt.Fprintf(c.Writer, "data: {}\n\n")
			flusher.Flush()
		}
	}
}

func notify(c *gin.Context) {
	idParam := c.Param("id")

	id, err := strconv.Atoi(idParam)
	if err != nil {
		c.JSON(400, gin.H{"error": "invalid id"})
		return
	}

	var todo Todo
	err = databaseConn.QueryRow(
		context.Background(),
		`SELECT id, title, description, due_date, status, created_at
		 FROM todos WHERE id=$1`,
		id,
	).Scan(
		&todo.ID,
		&todo.Title,
		&todo.Description,
		&todo.Due_Date,
		&todo.Status,
		&todo.Created_At,
	)

	if err != nil {
		c.JSON(404, gin.H{"error": "todo not found"})
		return
	}

	payload := fmt.Sprintf(
		`{"id": %d, "title": "%s", "status": "%s", "due_date": "%s"}`,
		todo.ID,
		derefString(todo.Title),
		derefString(todo.Status),
		formatDate(todo.Due_Date),
	)

	broadcast <- payload

	c.JSON(200, gin.H{
		"message":   "Alerte envoyée",
		"listeners": len(clients),
	})
}

func main() {
	// Get environment variables
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	connectionString = os.Getenv("POSTGRESQL_ADDON_URI")
	appName = os.Getenv("APP_NAME")
	if appName == "" {
		appName = "My awesome API"
	}

	// Connect to the database
	databaseConn, err := Connect(connectionString)
	if err != nil {
		log.Fatal(err)
	}
	defer databaseConn.Close(context.Background())

	// Create type and table
	rows, err := databaseConn.Query(context.Background(), "CREATE TYPE IF NOT EXISTS status AS ENUM ('pending', 'done');")
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()

	rows, err = databaseConn.Query(context.Background(), `CREATE TABLE IF NOT EXISTS todos (
														id          SERIAL PRIMARY KEY,
														title       VARCHAR(60) NOT NULL,
														description VARCHAR(255) NULL,
														due_date    DATE NULL,
														status	  status NOT NULL DEFAULT 'pending'
														created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
													);`)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()

	// Goroutine for alerts
	go func() {
		for {
			msg := <-broadcast
			for client := range clients {
				client <- msg
			}
		}
	}()

	// Start the server
	router := gin.Default()
	router.GET("/health", getHealth)
	router.GET("/todos", getTodos)
	router.POST("/todos", createTodo)
	router.PATCH("/todos/:id", editTodo)
	router.DELETE("/todos/:id", deleteTodo)
	router.GET("/todos/overdue", getOverdueTodos)
	router.GET("/alerts", alerts)
	router.POST("/todos/:id/notify", notify)
	router.Run("localhost:" + port)
}
