-- manyup tab completion for Clink (cmd.exe).
--
-- Clink turns cmd.exe into a readline-style prompt with completions:
--   https://github.com/chrisant996/clink
--   winget install --id chrisant996.Clink -e
--
-- Install:
--   if not exist "%LOCALAPPDATA%\clink" mkdir "%LOCALAPPDATA%\clink"
--   manyup completion clink > "%LOCALAPPDATA%\clink\manyup.lua"
--
-- Then open a new cmd window, or press Ctrl-X Ctrl-R to reload scripts.
-- Run `clink info` if your profile directory is somewhere else.
--
-- NOTE: this script calls `manyup` by name, so manyup must be on PATH for
-- completion to find it. Without that, Tab falls back to file completion.
--
-- The candidate list comes from `manyup __complete <words>`; this script only
-- decides which words to pass, so it stays correct as services are added.

local function trim(s)
    return (s:gsub("^%s+", ""):gsub("%s+$", ""))
end

-- Query manyup for the words typed so far. Returns a table of matches, or nil
-- when the binary is missing or produced nothing (so Clink falls back to files).
local function candidates(words)
    local parts = { "manyup", "__complete" }
    for _, word in ipairs(words) do
        -- Strip quotes rather than escaping them: service names and
        -- subcommands never contain them.
        parts[#parts + 1] = '"' .. word:gsub('"', "") .. '"'
    end
    parts[#parts + 1] = "2>nul"

    local ok, pipe = pcall(io.popen, table.concat(parts, " "), "r")
    if not ok or not pipe then
        return nil
    end

    local matches = {}
    for line in pipe:lines() do
        line = trim(line)
        if line ~= "" then
            matches[#matches + 1] = line
        end
    end
    pipe:close()

    return matches
end

-- Called for the word under the cursor. word_index is 1-based and points at
-- that word, so arguments before it are 2 .. word_index - 1 (skipping the
-- command name at index 1).
local function complete(word, word_index, line_state)
    local args = {}
    for i = 2, word_index - 1 do
        local previous = line_state:getword(i)
        if previous then
            args[#args + 1] = previous
        end
    end

    local matches = candidates(args)
    if not matches or #matches == 0 then
        return nil
    end

    if word == nil or word == "" then
        return matches
    end

    -- Filter here too so matching stays case-insensitive.
    local prefix = word:lower()
    local filtered = {}
    for _, match in ipairs(matches) do
        if match:lower():sub(1, #prefix) == prefix then
            filtered[#filtered + 1] = match
        end
    end
    return filtered
end

-- Five slots cover every manyup command; the longest is
-- `config set <service> <key> <value>`. Both spellings are registered so
-- completion works whether the user types `manyup` or `manyup.exe`.
for _, name in ipairs({ "manyup", "manyup.exe" }) do
    local matcher = clink.argmatcher(name)
    for _ = 1, 5 do
        matcher:addarg({ complete })
    end
end
