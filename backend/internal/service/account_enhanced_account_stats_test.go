//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 账号统计口径（account_stats）：billed 时账号计费 = 账号额度扣减，
// 且两者都按改写后 token × 渠道售价；raw 保持 rev14 行为（额度按真实 token 还原）。
func TestGatewayRecordUsage_AccountStatsMode(t *testing.T) {
	const (
		inputPrice  = 5e-6
		outputPrice = 25e-6
		writePrice  = 6.25e-6
		readPrice   = 0.6e-6
		accountRate = 1.5
	)
	// 真实拆分 input=100 / cache_read=9900，固定 80% 改写后 input=2000 / cache_read=8000。
	rewrittenTotal := 2000*inputPrice + 400*outputPrice + 1000*writePrice + 8000*readPrice
	rawChannelTotal := 100*inputPrice + 400*outputPrice + 1000*writePrice + 9900*readPrice

	for _, tc := range []struct {
		name       string
		mode       AccountStatsMode
		wantQuota  float64
		wantBilled bool
	}{
		{name: "billed", mode: AccountStatsModeBilled, wantQuota: rewrittenTotal * accountRate, wantBilled: true},
		{name: "raw", mode: AccountStatsModeRaw, wantQuota: rawChannelTotal * accountRate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
			billingRepo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
			svc := newGatewayRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})
			groupID := int64(931)
			in, out, write, read := inputPrice, outputPrice, writePrice, readPrice
			group := &Group{ID: groupID, Platform: PlatformAnthropic, RateMultiplier: 0.8,
				ModelPricing: []ChannelModelPricing{{
					Platform: PlatformAnthropic, Models: []string{"claude-opus-5.5"}, BillingMode: BillingModeToken,
					InputPrice: &in, OutputPrice: &out, CacheWritePrice: &write, CacheReadPrice: &read,
				}},
			}
			svc.channelService = newTestChannelServiceForStats(t, &Channel{ID: 8, Status: StatusActive}, groupID, PlatformAnthropic)
			svc.resolver = NewModelPricingResolver(svc.channelService, svc.billingService)
			rate := accountRate
			account := &Account{ID: 2331, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, RateMultiplier: &rate,
				Extra: map[string]any{
					"quota_limit": 2200.0,
					EnhancedControlKey: map[string]any{
						"cache_rate":    map[string]any{"mode": "fixed", "fixed_value": 80.0},
						"account_stats": map[string]any{"mode": string(tc.mode)},
					},
				}}

			err := svc.RecordUsage(context.Background(), &RecordUsageInput{
				Result: &ForwardResult{
					RequestID: "account_stats_" + tc.name,
					Model:     "claude-opus-5.5", UpstreamModel: "claude-opus-5.5",
					Usage:    ClaudeUsage{InputTokens: 100, OutputTokens: 400, CacheReadInputTokens: 9900, CacheCreationInputTokens: 1000},
					Duration: time.Second,
				},
				APIKey:  &APIKey{ID: 931, GroupID: &groupID, Group: group},
				User:    &User{ID: 931},
				Account: account,
			})
			require.NoError(t, err)
			log := usageRepo.lastLog
			require.NotNil(t, log)
			require.NotNil(t, billingRepo.lastCmd)
			require.Equal(t, 2000, log.InputTokens)
			require.Equal(t, 8000, log.CacheReadTokens)
			require.InDelta(t, rewrittenTotal, log.TotalCost, 1e-12)
			require.InDelta(t, QuantizeUsageBillingAmount(tc.wantQuota), billingRepo.lastCmd.AccountQuotaCost, 1e-12)

			if !tc.wantBilled {
				return
			}
			require.NotNil(t, log.AccountStatsCost)
			require.InDelta(t, rewrittenTotal, *log.AccountStatsCost, 1e-12)
			require.InDelta(t, billingRepo.lastCmd.AccountQuotaCost, *log.AccountStatsCost*accountRate, 1e-9,
				"后台账号计费（account_stats_cost × 账号倍率）必须等于账号额度扣减")
		})
	}
}
