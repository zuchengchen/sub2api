package repository

import "time"

func schedulerAstraVersion(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format("20060102150405.000000000")
}

// Both account projections change atomically. A delayed rebuild may still
// publish its IDs/revision, but must not restore an older OAuth model policy.
const schedulerWriteAstraAccountScript = `
local previous = redis.call('GET', KEYS[2])
local current = redis.call('GET', KEYS[1])
local function isNewer(raw)
  if not raw then return false end
  local ok, decoded = pcall(cjson.decode, raw)
  if ok and type(decoded) == 'table' and decoded.Platform == 'openai' and decoded.Type == 'oauth' then
    local version = decoded.astra_policy_version
    if type(version) == 'string' and version > ARGV[3] then
      return true
    end
  end
  return false
end
if isNewer(current) or isNewer(previous) then return previous end
redis.call('SET', KEYS[1], ARGV[1])
redis.call('SET', KEYS[2], ARGV[2])
return previous
`
