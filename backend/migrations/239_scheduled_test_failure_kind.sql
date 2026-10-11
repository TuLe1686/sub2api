-- 239: 定时测试失败子类（观察字段）。
-- 只记录 classification = 'failure' 的尝试子类，供面板展示与统计：
-- auth / rate_limited / upstream / network / business / unknown。
-- 该字段不参与连续超时计数、平台熔断、停用预算或保护动作，旧行回退为空串。

ALTER TABLE scheduled_test_results
    ADD COLUMN IF NOT EXISTS failure_kind VARCHAR(32) NOT NULL DEFAULT '';
