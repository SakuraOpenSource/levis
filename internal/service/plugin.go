package service

import (
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/SakuraOpenSource/levis/internal/model"
	"github.com/SakuraOpenSource/levis/internal/plugin"
	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
)

// maxConfigValueLen 是单项配置值的长度上限。
//
// 8 KiB 足够放下证书与私钥，同时挡住「把整个文件粘进来」这类误操作。
const maxConfigValueLen = 8 << 10

// PluginService 读写插件的启用状态、授权与配置。
//
// 它是 plugin.Manager 与数据库之间的那一层：Manager 不知道配置存在哪里，
// 只通过 ConfigProvider 接口拿值。
type PluginService struct {
	db *gorm.DB
}

// NewPluginService 构造 PluginService。
func NewPluginService(db *gorm.DB) *PluginService {
	return &PluginService{db: db}
}

// PluginConfig 返回插件的配置键值对。实现 plugin.ConfigProvider。
func (s *PluginService) PluginConfig(id string) (map[string]string, error) {
	var rows []model.PluginSetting
	if err := s.db.Where("plugin_id = ?", id).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		out[row.Key] = row.Value
	}
	return out, nil
}

// PluginScopes 返回管理员授予该插件的权限位。实现 plugin.ConfigProvider。
func (s *PluginService) PluginScopes(id string) ([]string, error) {
	state, err := s.state(id)
	if err != nil {
		return nil, err
	}
	return state.Scopes, nil
}

// EnabledPlugins 返回管理员此前启用的插件，供主程序启动时恢复。
func (s *PluginService) EnabledPlugins() (map[string]bool, error) {
	var rows []model.PluginState
	if err := s.db.Where("enabled = ?", true).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(rows))
	for _, row := range rows {
		out[row.PluginID] = true
	}
	return out, nil
}

// SetEnabled 记下管理员的启停意图。
//
// 只写库不动进程：拉起或停掉进程是 Manager 的事，两件事分开才能让「重启后
// 恢复」与「此刻启停」共用同一份记录。
func (s *PluginService) SetEnabled(id string, enabled bool) error {
	if err := plugin.ValidID(id); err != nil {
		return ErrBadRequest("%v", err)
	}
	return s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "plugin_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"enabled", "updated_at"}),
	}).Create(&model.PluginState{PluginID: id, Enabled: enabled, Scopes: model.ScopeList{}}).Error
}

// SetScopes 保存管理员授予的权限位。
func (s *PluginService) SetScopes(id string, scopes []string) error {
	if err := plugin.ValidID(id); err != nil {
		return ErrBadRequest("%v", err)
	}
	list, err := normalizePluginScopes(scopes)
	if err != nil {
		return err
	}
	return s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "plugin_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"scopes", "updated_at"}),
	}).Create(&model.PluginState{PluginID: id, Scopes: list}).Error
}

// ConfigField 是发给前端的配置字段定义。
//
// HasValue 而不是 Value 用于 secret 字段：密码与私钥存进去之后，API 永不回传
// 明文。前端据此渲染一个空密码框，提交空值表示「不修改」。这与 KYC 身份证号
// 打码、PasswordHash 带 json:"-" 是同一条纪律。
type ConfigField struct {
	Key      string         `json:"key"`
	Label    string         `json:"label"`
	Type     string         `json:"type"`
	Required bool           `json:"required"`
	Secret   bool           `json:"secret"`
	Hint     string         `json:"hint,omitempty"`
	Options  []SelectOption `json:"options,omitempty"`
	// Value 是当前值；secret 字段一律为空。未填写时回落到 manifest 的默认值。
	Value string `json:"value"`
	// HasValue 报告 secret 字段是否已存过值。
	HasValue bool `json:"has_value"`
}

// SelectOption 是下拉字段的一个选项。
type SelectOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// PluginDetail 是插件详情。
type PluginDetail struct {
	plugin.Snapshot
	// Granted 是管理员实际授予的权限位，与 Snapshot.Scopes（插件申请的）对照看。
	Granted []string      `json:"granted_scopes"`
	Config  []ConfigField `json:"config"`
	// Configured 报告必填项是否都已填写；未填全时前端提示「未配置」。
	Configured bool `json:"configured"`
	// ConfigSchemaReady 报告插件是否已经成功返回过 manifest。
	// false 时 Config 为空不代表插件没有配置项。
	ConfigSchemaReady bool `json:"config_schema_ready"`
}

// Detail 组装插件详情：运行状态 + manifest 里的字段定义 + 当前值。
func (s *PluginService) Detail(inst *plugin.Instance) (*PluginDetail, error) {
	id := inst.ID()
	values, err := s.PluginConfig(id)
	if err != nil {
		return nil, err
	}
	state, err := s.state(id)
	if err != nil {
		return nil, err
	}

	out := PluginDetail{
		Snapshot:          inst.Snapshot(),
		Granted:           state.Scopes,
		Config:            []ConfigField{},
		Configured:        true,
		ConfigSchemaReady: inst.Manifest() != nil,
	}
	if out.Granted == nil {
		out.Granted = []string{}
	}
	// Enabled 取库里的意图而不是实例的当前状态：插件反复崩溃已经停止重试时，
	// 管理员的意图仍然是「启用」，界面上的开关不该自己弹回去。
	out.Enabled = state.Enabled

	manifest := inst.Manifest()
	if manifest == nil {
		return &out, nil
	}
	for _, field := range manifest.GetConfig() {
		value := values[field.GetKey()]
		item := ConfigField{
			Key:      field.GetKey(),
			Label:    field.GetLabel(),
			Type:     fieldTypeName(field.GetType()),
			Required: field.GetRequired(),
			Secret:   field.GetSecret(),
			Hint:     field.GetHint(),
			Options:  selectOptions(field.GetOptions()),
			HasValue: value != "",
		}
		if !field.GetSecret() {
			item.Value = value
			if value == "" {
				// 没存过值时把 manifest 的默认值给前端当初始值。
				item.Value = field.GetDefaultValue()
			}
		}
		if field.GetRequired() && value == "" {
			out.Configured = false
		}
		out.Config = append(out.Config, item)
	}
	return &out, nil
}

// SaveConfig 保存插件配置，返回落库后的详情。
//
// 语义要点：
//   - 只处理 manifest 声明过的字段，未声明的键一律忽略 —— 否则前端传什么都
//     会被存下来，插件换版本后库里会攒下一堆无人认领的键。
//   - secret 字段提交空值表示「不修改」，因为读接口从不回传它的值，前端拿不到
//     原值可回填。非 secret 字段提交空值就是清空。
func (s *PluginService) SaveConfig(inst *plugin.Instance, values map[string]string) error {
	manifest := inst.Manifest()
	if manifest == nil {
		// 字段定义来自插件的 manifest，插件没跑起来就无从校验提交的键 ——
		// 此时静默存下去只会把值存成谁也不认领的键。如实报错，让管理员先启用。
		return ErrBadRequest("插件未运行，无法保存配置（配置项定义由插件自己提供）")
	}
	fields := manifest.GetConfig()
	if len(fields) == 0 {
		return ErrBadRequest("该插件没有可配置项")
	}

	var rows []model.PluginSetting
	for _, field := range fields {
		raw, ok := values[field.GetKey()]
		if !ok {
			continue
		}
		value := strings.TrimSpace(raw)
		if value == "" && field.GetSecret() {
			// 空值表示不修改，跳过而不是清空。要清空得另做一个显式动作。
			continue
		}
		if len(value) > maxConfigValueLen {
			return ErrBadRequest("%s 的值过长（上限 %d 字节）", field.GetLabel(), maxConfigValueLen)
		}
		rows = append(rows, model.PluginSetting{
			PluginID: inst.ID(),
			Key:      field.GetKey(),
			Value:    value,
		})
	}
	if len(rows) == 0 {
		return nil
	}
	return s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "plugin_id"}, {Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"value", "updated_at"}),
	}).Create(&rows).Error
}

// FrontendConfig 返回插件前端配置，根据 manifest 区分 secret 字段。
func (s *PluginService) FrontendConfig(id string, manifest *pb.Manifest) (map[string]any, error) {
	values, err := s.PluginConfig(id)
	if err != nil {
		return nil, err
	}
	out := make(map[string]any, len(values)*2)
	for k, v := range values {
		out[k] = v
	}
	if manifest != nil {
		for _, field := range manifest.GetConfig() {
			if field.GetSecret() {
				out[field.GetKey()+"_set"] = values[field.GetKey()] != ""
			}
		}
	}
	// 兼容旧版 manifest 为 nil 的情况：枚举所有 keys 生成 _set 标记
	if manifest == nil {
		for k, v := range values {
			out[k+"_set"] = v != ""
		}
	}
	return out, nil
}

// SaveFrontendConfig 保存前端声明的插件配置，根据 manifest 校验字段。
func (s *PluginService) SaveFrontendConfig(id string, manifest *pb.Manifest, values map[string]string) error {
	allowed := map[string]bool{}
	if manifest != nil {
		for _, field := range manifest.GetConfig() {
			allowed[field.GetKey()] = true
		}
	}
	rows := make([]model.PluginSetting, 0, len(values))
	for key, raw := range values {
		if len(allowed) > 0 && !allowed[key] {
			return ErrBadRequest("不支持的插件配置项: %s", key)
		}
		value := strings.TrimSpace(raw)
		if value == "" {
			skipSecret := false
			if manifest != nil {
				for _, field := range manifest.GetConfig() {
					if field.GetKey() == key && field.GetSecret() {
						skipSecret = true
						break
					}
				}
			}
			if skipSecret {
				continue
			}
		}
		if len(value) > maxConfigValueLen {
			return ErrBadRequest("配置项 %s 的值过长（上限 %d 字节）", key, maxConfigValueLen)
		}
		rows = append(rows, model.PluginSetting{PluginID: id, Key: key, Value: value})
	}
	if len(rows) == 0 {
		return nil
	}
	return s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "plugin_id"}, {Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"value", "updated_at"}),
	}).Create(&rows).Error
}

// DeletePluginData 清掉某个插件在库里的全部痕迹。插件从磁盘移除后调用。
func (s *PluginService) DeletePluginData(id string) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("plugin_id = ?", id).Delete(&model.PluginSetting{}).Error; err != nil {
			return err
		}
		return tx.Where("plugin_id = ?", id).Delete(&model.PluginState{}).Error
	})
}

// state 读插件的启用与授权记录，没有记录时返回零值。
//
// 「从未配置过」是正常状态，不是错误：插件刚被放进目录时库里什么都没有。
func (s *PluginService) state(id string) (*model.PluginState, error) {
	var row model.PluginState
	// Limit(1).Find 而不是 First：后者在没有记录时会返回
	// gorm.ErrRecordNotFound，并在日志里留下一行 error 级别的噪音。
	result := s.db.Where("plugin_id = ?", id).Limit(1).Find(&row)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return &model.PluginState{PluginID: id, Scopes: model.ScopeList{}}, nil
	}
	return &row, nil
}

// selectOptions 把 protobuf 的选项列表转成接口层结构。
func selectOptions(in []*pb.SelectOption) []SelectOption {
	if len(in) == 0 {
		return nil
	}
	out := make([]SelectOption, 0, len(in))
	for _, item := range in {
		out = append(out, SelectOption{Value: item.GetValue(), Label: item.GetLabel()})
	}
	return out
}

// fieldTypeName 把 protobuf 枚举映射为前端用的类型名。
//
// 前端按这个值分发到对应的 shadcn 组件；未知类型退化为文本框，这样插件用了
// 更新版本的契约时界面也不会整个渲染不出来。
func fieldTypeName(t pb.FieldType) string {
	switch t {
	case pb.FieldType_FIELD_TYPE_NUMBER:
		return "number"
	case pb.FieldType_FIELD_TYPE_BOOL:
		return "bool"
	case pb.FieldType_FIELD_TYPE_SELECT:
		return "select"
	case pb.FieldType_FIELD_TYPE_TEXTAREA:
		return "textarea"
	}
	return "text"
}
