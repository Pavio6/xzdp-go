-- KEYS[1]: stockKey, KEYS[2]: orderSetKey
-- ARGV[1]: userId, ARGV[2]: voucherId
local stockKey = KEYS[1]
local orderSetKey = KEYS[2]
local userId = ARGV[1]
local voucherId = ARGV[2]

local stock = tonumber(redis.call("get", stockKey))
if not stock or stock <= 0 then
  return 1
end
if redis.call("sismember", orderSetKey, userId) == 1 then
  return 2
end

redis.call("decr", stockKey)
redis.call("sadd", orderSetKey, userId)
return 0
