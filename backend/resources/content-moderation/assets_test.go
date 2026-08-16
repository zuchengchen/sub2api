package contentmoderationassets

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLegacyPromptAuditCandidateAsset(t *testing.T) {
	asset, err := Load(LegacyPromptAuditV1)
	require.NoError(t, err)
	require.False(t, asset.Manifest.EnabledByDefault)
	require.Equal(t, "99c8e4bf7564823bafbab369acab6539e734c1bb", asset.Manifest.SourceCommit)
	require.Equal(t, "layer1-high-confidence-keywords.json", asset.Manifest.Layer1.File)
	require.Equal(t, "layer2-candidate-keywords.json", asset.Manifest.Layer2.File)
	require.Len(t, asset.Layer1, 927)
	require.Len(t, asset.Layer2, 290)
	require.Len(t, asset.Layer1Demotions, 99)
	require.Len(t, asset.Layer1Suppressions, 29)
	require.Len(t, asset.Manifest.CandidateEndpoints, 1)
	require.False(t, asset.Manifest.CandidateEndpoints[0].Enabled)
	require.Empty(t, asset.Manifest.CandidateEndpoints[0].Token)
}

func TestLegacyPromptAuditCandidateAssetAppliesLayerAssignments(t *testing.T) {
	asset, err := Load(LegacyPromptAuditV1)
	require.NoError(t, err)

	for _, keyword := range []string{
		"制作病毒",
		"write a virus",
		"ransomware-as-a-service",
		"production ready exploit",
		"制作炸弹",
		"写一封钓鱼信",
		"write a phishing email",
		"破解他人账号",
		"植入后门",
		"Forge 可执行见证",
	} {
		require.Contains(t, asset.Layer1, keyword)
	}

	layer2 := make(map[string]struct{}, len(asset.Layer2))
	for _, keyword := range asset.Layer2 {
		layer2[candidatePrefilterKey(keyword)] = struct{}{}
	}
	demotions := make(map[string]struct{}, len(asset.Layer1Demotions))
	for _, keyword := range asset.Layer1Demotions {
		demotions[normalizedCandidateTerm(keyword)] = struct{}{}
	}
	for _, keyword := range []string{
		"mcp__ida",
		"remote access trojan",
		"降级攻击",
		"obfuscator",
		"firmware reverse engineering",
		"pass-the-hash",
		"zero_day",
		"destructive payload",
		"metamorphic",
		"do anything now",
		"make a bomb",
		"malicious payload",
		"session hijacking",
		"opposite mode",
		"bypass content filter",
		"powershell -enc",
		"powershell -EncodedCommand",
		"bash -i >& /dev/tcp",
		"Set-MpPreference -DisableRealtimeMonitoring",
		"键盘记录器",
		"keystroke logger",
		"钓鱼邮件",
		"phishing email",
		"重放权杖",
		"token replay",
		"恶意 App",
		"malicious app",
		"破解密码",
		"crack password",
		"留后门",
		"受害者名单",
		"忽略先前指令",
		"ignore all previous instructions",
		"ignore prior instructions",
		"disregard the above",
		"忘记之前的所有指令",
	} {
		require.NotContains(t, asset.Layer1, keyword)
		require.Contains(t, layer2, candidatePrefilterKey(keyword))
	}
	for _, keyword := range []string{
		"metamorphic",
		"do anything now",
		"make a bomb",
		"malicious payload",
		"session hijacking",
		"opposite mode",
		"bypass content filter",
		"powershell -enc",
		"powershell -EncodedCommand",
		"bash -i >& /dev/tcp",
		"Set-MpPreference -DisableRealtimeMonitoring",
		"键盘侧录",
		"键盘记录器",
		"键盘纪录程序",
		"keystroke logger",
		"keystroke capture",
		"钓鱼邮件",
		"phishing email",
		"重放权杖",
		"token replay",
		"恶意 App",
		"malicious app",
		"破解密码",
		"crack password",
		"留后门",
		"受害者名单",
		"忽略先前指令",
		"忽略以上指令",
		"忘记你的规则",
		"无视你的限制",
		"ignore all previous instructions",
		"disregard your rules",
		"forget your guidelines",
		"override your instructions",
		"ignore prior instructions",
		"ignore the above directions",
		"ignore above instructions",
		"forget your instructions",
		"disregard the above",
		"disregard your instructions",
		"忽略之前的所有指令",
		"无视之前的所有指令",
		"忘记之前的所有指令",
	} {
		require.Contains(t, demotions, normalizedCandidateTerm(keyword))
	}

	for _, keyword := range []string{"hax0r", "sh3ll", "sp00f", "zer0day", "loader service"} {
		require.NotContains(t, asset.Layer1, keyword)
		require.NotContains(t, layer2, candidatePrefilterKey(keyword))
	}
	require.Contains(t, asset.Layer1Suppressions, "loader service")
}
