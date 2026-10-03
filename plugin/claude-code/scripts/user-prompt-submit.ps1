#!/usr/bin/env pwsh
# Engram - Windows-native UserPromptSubmit hook for Claude Code
#
# Optional fallback for enterprise Windows environments where Git Bash/MSYS2
# fork emulation is slowed or blocked by Defender/EDR. Keep this script small
# and dependency-free; it must never block prompt submission.

# Ensure UTF-8 output so JSON payloads with non-ASCII characters are not
# mangled when Claude Code reads this hook's stdout. Without this, Windows
# defaults to the system codepage (e.g. CP1252/CP850) which corrupts
# multi-byte characters in the systemMessage JSON (issue #421).
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
[Console]::InputEncoding  = [System.Text.Encoding]::UTF8

$ErrorActionPreference = 'SilentlyContinue'

function Write-EmptyHookResponse {
  Write-Output '{}'
}

# Emit the first-message ToolSearch bootstrap.
#
# Uses hookSpecificOutput.additionalContext (NOT systemMessage): on a
# UserPromptSubmit hook, only stdout/additionalContext enters the model's
# context. A systemMessage payload is rendered to the terminal as
# "UserPromptSubmit says: ..." (issue #145) and never reaches the model.
#
# The select list carries BOTH tool-name prefixes: mcp__plugin_engram_engram__*
# for plugin/marketplace installs and mcp__engram__* for direct MCP-server
# installs. ToolSearch select: returns whichever names exist and silently
# ignores the rest, so one list covers both install modes (issue #534).
function Write-ToolSearchMessage {
  $message = "CRITICAL FIRST ACTION - Execute this ToolSearch NOW before responding to the user:`nselect:mcp__plugin_engram_engram__mem_save,mcp__plugin_engram_engram__mem_search,mcp__plugin_engram_engram__mem_context,mcp__plugin_engram_engram__mem_session_summary,mcp__plugin_engram_engram__mem_session_start,mcp__plugin_engram_engram__mem_session_end,mcp__plugin_engram_engram__mem_get_observation,mcp__plugin_engram_engram__mem_suggest_topic_key,mcp__plugin_engram_engram__mem_capture_passive,mcp__plugin_engram_engram__mem_save_prompt,mcp__plugin_engram_engram__mem_update,mcp__plugin_engram_engram__mem_current_project,mcp__plugin_engram_engram__mem_judge,mcp__plugin_engram_engram__mem_doctor,mcp__plugin_engram_engram__mem_review,mcp__plugin_engram_engram__mem_pin,mcp__plugin_engram_engram__mem_unpin,mcp__engram__mem_save,mcp__engram__mem_search,mcp__engram__mem_context,mcp__engram__mem_session_summary,mcp__engram__mem_session_start,mcp__engram__mem_session_end,mcp__engram__mem_get_observation,mcp__engram__mem_suggest_topic_key,mcp__engram__mem_capture_passive,mcp__engram__mem_save_prompt,mcp__engram__mem_update,mcp__engram__mem_current_project,mcp__engram__mem_judge,mcp__engram__mem_doctor,mcp__engram__mem_review,mcp__engram__mem_pin,mcp__engram__mem_unpin`n`nAfter loading tools, call mem_context to check for prior session history before responding."
  [PSCustomObject]@{
    hookSpecificOutput = [PSCustomObject]@{
      hookEventName     = 'UserPromptSubmit'
      additionalContext = $message
    }
  } | ConvertTo-Json -Compress
}

function Resolve-EngramProject {
  param(
    [string]$EngramUrl,
    [string]$Cwd
  )
  if ([string]::IsNullOrWhiteSpace($Cwd)) { return $null }
  try {
    $encodedCwd = [System.Uri]::EscapeDataString($Cwd)
    $resolution = Invoke-RestMethod -Method Get -Uri "$EngramUrl/project/current?cwd=$encodedCwd" -TimeoutSec 1
    $projectProperty = @($resolution.PSObject.Properties | Where-Object { $_.Name -ceq 'project' })
    $sourceProperty = @($resolution.PSObject.Properties | Where-Object { $_.Name -ceq 'project_source' })
    if ($projectProperty.Count -ne 1 -or $sourceProperty.Count -ne 1 -or $projectProperty[0].Value -isnot [string] -or $sourceProperty[0].Value -isnot [string]) {
      return $null
    }
    $project = $projectProperty[0].Value
    $source = $sourceProperty[0].Value
    $validSources = @('config', 'git_remote', 'git_root', 'git_child', 'dir_basename', 'process_override')
    if ([string]::IsNullOrWhiteSpace($project) -or $validSources -cnotcontains $source -or $null -ne $resolution.PSObject.Properties['error_hint']) {
      return $null
    }
    return $project
  } catch {
    return $null
  }
}

function Invoke-EngramPromptPersist {
  param(
    [string]$EngramUrl,
    [string]$SessionId,
    [string]$Project,
    [string]$Prompt
  )
  # Fail-silent and bounded: a short timeout keeps a slow/unreachable server
  # from stalling prompt submission, and any error is swallowed.
  if ([string]::IsNullOrWhiteSpace($Prompt) -or [string]::IsNullOrWhiteSpace($SessionId) -or [string]::IsNullOrWhiteSpace($Project)) { return }
  try {
    $body = [PSCustomObject]@{
      session_id = $SessionId
      project    = $Project
      content    = $Prompt
    } | ConvertTo-Json -Compress
    $null = Invoke-RestMethod -Method Post -Uri "$EngramUrl/prompts" `
      -ContentType 'application/json' -Body $body -TimeoutSec 1
  } catch { }
}

try {
  $engramPort = if ($env:ENGRAM_PORT) { $env:ENGRAM_PORT.Trim() } else { '7437' }
  $port = 0
  $validPort = $engramPort -cmatch '^[0-9]+$' -and [int]::TryParse($engramPort, [ref]$port) -and $port -ge 1 -and $port -le 65535
  $engramUrl = "http://127.0.0.1:$port"

  $inputJson = [Console]::In.ReadToEnd()
  try {
    if (-not $inputJson.TrimStart().StartsWith('{')) { throw 'Expected hook object' }
    $payload = $inputJson | ConvertFrom-Json -ErrorAction Stop
  } catch { $payload = $null }
  $sessionID = $payload.session_id
  $cwd = $payload.cwd
  $prompt = $payload.prompt
  $effectiveID = $null
  if ($validPort -and $sessionID -is [string] -and -not [string]::IsNullOrWhiteSpace($sessionID) -and $cwd -is [string] -and -not [string]::IsNullOrWhiteSpace($cwd)) {
    # Registration and persistence share LOCAL authority. Restore process-only
    # environment even on child failure; never modify user/global settings.
    $previousUrl = $env:ENGRAM_URL
    try {
      $env:ENGRAM_URL = $engramUrl
      $global:LASTEXITCODE = 0
      $ackJson = $inputJson | engram hook claude-session-register 2>$null
      if ($LASTEXITCODE -eq 0) {
        $ackText = $ackJson -join "`n"
        if (-not $ackText.TrimStart().StartsWith('{')) { throw 'Expected acknowledgement object' }
        $ack = $ackText | ConvertFrom-Json -ErrorAction Stop
        $idProperty = @($ack.PSObject.Properties | Where-Object { $_.Name -ceq 'id' })
        if ($ack -is [PSCustomObject] -and $idProperty.Count -eq 1 -and $idProperty[0].Value -is [string] -and -not [string]::IsNullOrWhiteSpace($idProperty[0].Value)) {
          $effectiveID = $idProperty[0].Value
        }
      }
    } catch { } finally { $env:ENGRAM_URL = $previousUrl }
  }
  if ($null -ne $effectiveID) {
    $project = Resolve-EngramProject -EngramUrl $engramUrl -Cwd $cwd
    Invoke-EngramPromptPersist -EngramUrl $engramUrl -SessionId $effectiveID -Project $project -Prompt $prompt
  }
  # The host-keyed marker is ephemeral UI state, never persistence authority.
  if ($sessionID -isnot [string] -or [string]::IsNullOrWhiteSpace($sessionID)) { $sessionID = "windows-$PID" }

  $safeSessionID = $sessionID -replace '[^a-zA-Z0-9_-]', '_'
  $stateFile = Join-Path ([IO.Path]::GetTempPath()) "engram-claude-$safeSessionID-tools-loaded"

  if (-not (Test-Path -LiteralPath $stateFile)) {
    New-Item -ItemType File -Path $stateFile -Force | Out-Null
    Write-ToolSearchMessage
    exit 0
  }

  Write-EmptyHookResponse
  exit 0
} catch {
  Write-EmptyHookResponse
  exit 0
}
