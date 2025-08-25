package main

import (
	"os"
	"context"

	"github.com/go-redis/redis/v8"
)

var ctx = context.Background()

func main() {
	host := os.Getenv("LISTEN")
	if len(host) == 0 {
		host = "localhost:9999"
	}
        user := os.Getenv("USERNAME")
	pass := os.Getenv("REDIS_PASSWORD")
	if len(pass) == 0 {
		pass := os.Getenv("PASSWORD")
	}
	client := redis.NewClient(&redis.Options{ Addr: host, Username: user, Password: pass, })
	role, err := client.Do(ctx,"role").Result()
	if err != nil {
		os.Exit(1)
	}
	status := role.([]interface{})
	currentRole := status[0]
	if currentRole == "master" {
		os.Exit(0)
	}
	if currentRole == "slave" && status[3] == "connected" {
		os.Exit(0)
	}
	os.Exit(127)
}
