package my_sites

import "strings"

// 分组的模型厂商并不等于账号协议；能力响应与创建校验共用这一份定义。
type GroupTypeOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type accountProtocol struct {
	GroupTypeOption
	concurrency int
	poolMode    bool
	passthrough string
	tier        string
}

var accountProtocols = []accountProtocol{
	{GroupTypeOption{"openai", "OpenAI"}, 1000, true, "openai_passthrough", ""},
	{GroupTypeOption{"anthropic", "Anthropic"}, 1000, true, "anthropic_passthrough", ""},
	{GroupTypeOption{"gemini", "Gemini"}, 1000, true, "", "aistudio_free"},
	{GroupTypeOption{"antigravity", "Antigravity"}, 10, false, "", ""},
}

func findAccountProtocol(value string) (accountProtocol, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, protocol := range accountProtocols {
		if protocol.ID == value {
			return protocol, true
		}
	}
	return accountProtocol{}, false
}

func accountGroupTypeOptions() []GroupTypeOption {
	options := make([]GroupTypeOption, 0, len(accountProtocols))
	for _, protocol := range accountProtocols {
		options = append(options, protocol.GroupTypeOption)
	}
	return options
}

func resolveAccountGroupType(cached, requested string, newAPIUpstream bool) (string, error) {
	cached = strings.ToLower(strings.TrimSpace(cached))
	requested = strings.ToLower(strings.TrimSpace(requested))
	if requested != "" {
		if _, ok := findAccountProtocol(requested); !ok {
			return "", requestError(ErrorGroupTypeInvalid)
		}
	}
	// Sub2API 已知协议仍由其分组持有；New API 允许用户覆盖厂商推断。
	if _, ok := findAccountProtocol(cached); ok && (!newAPIUpstream || requested == "") {
		return cached, nil
	}
	if requested != "" {
		return requested, nil
	}
	return "", requestError(ErrorGroupTypeRequired)
}
