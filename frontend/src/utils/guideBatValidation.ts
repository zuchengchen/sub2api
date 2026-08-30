export interface BatValidationIssue {
  code: string
  message: string
}

const expectedDomains = [
  'https://mofa.love.gd',
  'https://key66.vip',
  'https://mofayaoshipu.cc.cd',
  'https://key66.cc.cd',
]

function addIssue(issues: BatValidationIssue[], code: string, message: string) {
  issues.push({ code, message })
}

function executableLines(source: string): string[] {
  return source
    .split(/\r?\n/)
    .filter((line) => !/^\s*(?:#|rem\b|::)/i.test(line))
}

export function validateGuideBat(source: string): BatValidationIssue[] {
  const issues: BatValidationIssue[] = []
  const executable = executableLines(source).join('\n')

  if ([...source].some((character) => character.charCodeAt(0) > 0x7f)) {
    addIssue(issues, 'non-ascii', 'The downloadable BAT must remain ASCII.')
  }

  const domainBlock = source.match(/\$domains\s*=\s*@\(([\s\S]*?)\r?\n\)/)?.[1] || ''
  const configuredDomains = [...domainBlock.matchAll(/'(https:\/\/[^']+)'/g)].map((match) => match[1])
  if (JSON.stringify(configuredDomains) !== JSON.stringify(expectedDomains)) {
    addIssue(issues, 'domain-list', 'The exact approved domain list or order changed.')
  }

  const requiredFragments: Array<[string, string, string]> = [
    ['fixed-host', "$hostIp = '15.204.82.11'", 'The approved fixed hosts address is missing.'],
    ['health-path', "+ '/health'", 'The public /health path is missing.'],
    ['three-attempts', '$MeasuredAttempts = 3', 'The script must use three measured attempts.'],
    ['median', '($times | Sort-Object)[1]', 'The three-sample median selection is missing.'],
    ['strict-health', "Health check response was not exactly status=ok.", 'Strict health JSON validation is missing.'],
    ['mutex', 'Global\\Sub2API-CodexBaseUrlSelector', 'The single-instance mutex is missing.'],
    ['config-parser', 'function Get-CodexConfigTarget', 'The scoped provider parser is missing.'],
    ['host-only-update', 'function Set-UrlHostOnly', 'The host-only URL updater is missing.'],
    ['atomic-replace', '[IO.File]::Replace', 'Atomic file replacement is missing.'],
    ['timestamp-backup', '.codex-speed.$runId.bak', 'Timestamped backups are missing.'],
    ['rollback', 'function Restore-OwnedChange', 'Verified rollback support is missing.'],
    ['config-race-check', 'Codex config.toml changed after preflight', 'Config change detection is missing.'],
    ['hosts-race-check', 'The hosts file changed after preflight', 'Hosts change detection is missing.'],
    ['hosts-alias-safety', 'shares a line with other aliases', 'Shared hosts-line protection is missing.'],
    ['elevation-path-safety', '$env:BATCH_PATH + [char]34', 'Safe elevation path forwarding is missing.'],
    ['final-config-hash', '$verifiedConfig.Hash -ne $expectedConfigHash', 'Final config byte verification is missing.'],
    ['final-hosts-hash', '$expectedHostsHash = if ($hostsTouched)', 'Final hosts byte verification is missing.'],
  ]

  for (const [code, fragment, message] of requiredFragments) {
    if (!source.includes(fragment)) addIssue(issues, code, message)
  }

  if ((source.match(/Invoke-HealthSample -Origin \$origin -Timeout \$Timeout/g) || []).length < 2) {
    addIssue(issues, 'warmup-and-measurement', 'A warm-up and measured health calls are both required.')
  }

  if (!/if \(\$properties\.Count -ne 1[\s\S]*\$payload\.status -ne 'ok'\)/.test(source)) {
    addIssue(issues, 'health-shape', 'The health response must contain only status=ok.')
  }

  if (!/\$response\.Content -cnotmatch '\^\\s\*\\\{[\s\S]*"status"[\s\S]*"ok"/.test(source)) {
    addIssue(issues, 'health-raw-shape', 'The raw health response shape check is missing.')
  }

  if (!/Restore-OwnedChange -Path \$configPath[\s\S]*Restore-OwnedChange -Path \$hostsPath/.test(source)) {
    addIssue(issues, 'two-file-rollback', 'Both config and hosts rollback paths are required.')
  }

  if (/^\s*model_provider\s*=/im.test(executable) || /Set-TomlSetting[^\r\n]*model_provider/i.test(executable)) {
    addIssue(issues, 'model-provider-write', 'The script must not assign model_provider.')
  }

  if (/\b(?:api[_ -]?key|authorization|bearer|env_key|wire_api|requires_openai_auth|experimental_bearer_token)\b/i.test(executable)) {
    addIssue(issues, 'credential-reference', 'The script must not read or write credentials or auth settings.')
  }

  if (/Set-TomlSetting/i.test(executable)) {
    addIssue(issues, 'unscoped-toml-write', 'The old unscoped TOML setter must not return.')
  }

  return issues
}
