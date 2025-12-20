package utils

import "time"

type RedisData struct {
	ExpireTime time.Time   `json:"expireTime"`
	Data       interface{} `json:"data"`
}
