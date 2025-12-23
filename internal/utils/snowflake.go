package utils

import (
	"fmt"
	"sync"
	"time"
)

/*
简易雪花算法（Snowflake）实现：生成分布式唯一ID（int64）

64位结构（常见做法）：
- 1bit   符号位：固定为0（保证正数）
- 41bit  时间戳：毫秒级（相对自定义纪元 epoch）
- 10bit  机器ID： workerID(0~1023) 
- 12bit  序列号：同一毫秒内自增（0~4095）
*/

const (
	// 自定义纪元（epoch）：2025-01-01 00:00:00 UTC（你也可以换成项目上线时间）
	// 这样做的目的是：减少时间戳占用，使ID更紧凑
	epochMs int64 = 1735689600000

	workerIDBits uint8 = 10                                      // 机器ID占用10位
	sequenceBits uint8 = 12                                      // 序列号占用12位
	timeBits     uint8 = 41                                      // 时间戳占用41位（这里不直接用，但用于理解）
	maxWorkerID        = int64(-1) ^ (int64(-1) << workerIDBits) // 1023
	maxSequence2       = int64(-1) ^ (int64(-1) << sequenceBits) // 4095

	workerIDShift = sequenceBits
	timeShift     = sequenceBits + workerIDBits
)

// Snowflake 生成器
type Snowflake struct {
	mu           sync.Mutex
	workerID     int64 // 机器ID：0~1023
	lastTimeMs   int64 // 上一次生成ID的毫秒时间戳
	sequence     int64 // 同一毫秒内的序列号
	timeRollback int64 // 允许的“时间回拨”容忍毫秒数（简易处理用）
}

// NewSnowflake 创建一个雪花生成器
func NewSnowflake(workerID int64) (*Snowflake, error) {
	if workerID < 0 || workerID > maxWorkerID {
		return nil, fmt.Errorf("workerID 必须在 [0, %d] 范围内", maxWorkerID)
	}
	return &Snowflake{
		workerID:     workerID,
		lastTimeMs:   -1,
		sequence:     0,
		timeRollback: 5, // 简易策略：允许最多回拨 5ms（生产环境建议更严谨）
	}, nil
}

// NextID 生成下一个唯一ID
func (s *Snowflake) NextID() (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := currentMs()

	// 1) 处理时钟回拨（简易版）
	// 如果检测到当前时间 < 上次时间：说明系统时间被回拨了
	if now < s.lastTimeMs {
		diff := s.lastTimeMs - now
		// 简易策略：小回拨就等待追平；回拨太大就报错
		if diff <= s.timeRollback {
			now = waitUntil(s.lastTimeMs)
		} else {
			return 0, fmt.Errorf("检测到时钟回拨：回拨 %dms，拒绝生成ID", diff)
		}
	}

	// 2) 如果还是同一毫秒：序列号 +1
	if now == s.lastTimeMs {
		s.sequence = (s.sequence + 1) & maxSequence2
		// 序列号用完（超过4095），等待进入下一毫秒
		if s.sequence == 0 {
			now = waitUntil(s.lastTimeMs + 1)
		}
	} else {
		// 3) 不同毫秒：序列号归零
		s.sequence = 0
	}

	s.lastTimeMs = now

	// 4) 组装ID： (时间戳<<timeShift) | (workerID<<workerIDShift) | sequence
	ts := now - epochMs
	if ts < 0 {
		return 0, fmt.Errorf("当前时间早于epoch，ts=%d", ts)
	}
	// 理论上41位时间戳可用约69年；这里做个简单上限检查（可选）
	if ts >= (int64(1) << timeBits) {
		return 0, fmt.Errorf("时间戳超出可表示范围：ts=%d", ts)
	}

	id := (ts << timeShift) | (s.workerID << workerIDShift) | s.sequence
	return id, nil
}

// currentMs 获取当前毫秒时间戳
func currentMs() int64 {
	return time.Now().UnixNano() / int64(time.Millisecond)
}

// waitUntil 等待直到时间戳 >= targetMs
func waitUntil(targetMs int64) int64 {
	for {
		now := currentMs()
		if now >= targetMs {
			return now
		}
		// 小睡一下减少CPU空转（简易）
		time.Sleep(100 * time.Microsecond)
	}
}

