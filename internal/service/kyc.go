package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"

	"github.com/SakuraOpenSource/levis/internal/model"
	"github.com/SakuraOpenSource/levis/internal/plugin"
	"github.com/SakuraOpenSource/levis/internal/storage"
	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
)

// kycCategory 是证件照在 uploads 下的分类目录名。
const kycCategory = "kyc"

// MaxPhotoBytes 是单张证件照的上限（8 MiB）。手机直出的照片通常 2-5 MiB。
const MaxPhotoBytes = 8 << 20

// allowedPhotoMimes 是证件照允许的类型。
//
// 白名单而非黑名单：证件照要塞进 <img> 内联展示，一旦放进 HTML、SVG 这类
// 可执行内容，就等于给了一个同源脚本注入点。
var allowedPhotoMimes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/webp": true,
}

// 证件照的两面。
const (
	SideFront = "front" // 人像面
	SideBack  = "back"  // 国徽面
)

// KYCService 处理实名认证的提交与审核。
type KYCService struct {
	db      *gorm.DB
	store   *storage.Store
	plugins *plugin.Manager
}

// NewKYCService 构造 KYCService。
func NewKYCService(db *gorm.DB, store *storage.Store, plugins ...*plugin.Manager) *KYCService {
	var manager *plugin.Manager
	if len(plugins) > 0 {
		manager = plugins[0]
	}
	return &KYCService{db: db, store: store, plugins: manager}
}

// StartExternal 发起第三方实名认证。身份证号只在本次 RPC 中使用，插件与
// 主程序均不把它写进日志；数据库保留现有 KYC 记录是为了查询与反重复认证。
func (s *KYCService) StartExternal(ctx context.Context, userID uint, name, idNumber string) (*model.Verification, *pb.StartKYCReply, error) {
	if s.plugins == nil {
		return nil, nil, ErrUnavailable("未启用实名认证插件")
	}
	inst := s.plugins.KYCPlugin()
	if inst == nil {
		return nil, nil, ErrUnavailable("没有可用的实名认证插件")
	}
	name, err := ValidateRealName(name)
	if err != nil {
		return nil, nil, err
	}
	idNumber, err = ValidateIDNumber(idNumber)
	if err != nil {
		return nil, nil, err
	}
	if existing, err := s.find(userID); err != nil {
		return nil, nil, err
	} else if existing != nil && existing.Status == model.KYCApproved {
		return nil, nil, ErrConflict("已通过实名认证，如需变更请联系客服")
	}
	reply, err := s.plugins.StartKYC(ctx, inst.ID(), &pb.StartKYCRequest{Name: name, IdCard: idNumber})
	if err != nil {
		return nil, nil, ErrUnavailable("发起实名认证失败: %v", err)
	}
	if reply.GetCertifyId() == "" {
		return nil, nil, ErrUnavailable("实名认证插件未返回 certify_id")
	}
	now := time.Now().UTC()
	record := model.Verification{UserID: userID, RealName: name, IDNumber: idNumber, Status: model.KYCPending, SubmittedAt: now, PluginID: inst.ID(), CertifyID: reply.GetCertifyId()}
	if existing, err := s.find(userID); err == nil && existing != nil {
		record.ID = existing.ID
		record.CreatedAt = existing.CreatedAt
	}
	if err := s.db.Save(&record).Error; err != nil {
		return nil, nil, err
	}
	return &record, reply, nil
}

// QueryExternal 查询第三方认证状态并在通过时更新本地 KYC 状态。
func (s *KYCService) QueryExternal(ctx context.Context, userID uint) (*model.Verification, string, error) {
	record, err := s.find(userID)
	if err != nil || record == nil {
		if err != nil {
			return nil, "", err
		}
		return nil, "", ErrNotFound("尚未发起实名认证")
	}
	if record.PluginID == "" || record.CertifyID == "" {
		return record, "", ErrConflict("当前认证记录不是第三方认证流程")
	}
	if s.plugins == nil {
		return nil, "", ErrUnavailable("实名认证插件不可用")
	}
	reply, err := s.plugins.QueryKYC(ctx, record.PluginID, &pb.QueryKYCRequest{CertifyId: record.CertifyID})
	if err != nil {
		return nil, "", ErrUnavailable("查询实名认证失败: %v", err)
	}
	passed := strings.ToUpper(strings.TrimSpace(reply.GetPassed()))
	if passed == "T" && record.Status != model.KYCApproved {
		now := time.Now().UTC()
		if err := s.db.Model(record).Updates(map[string]any{"status": model.KYCApproved, "reviewed_at": now, "reviewed_by": 0, "reject_reason": ""}).Error; err != nil {
			return nil, "", err
		}
		record.Status = model.KYCApproved
		record.ReviewedAt = &now
	} else if passed == "F" && record.Status == model.KYCPending {
		if err := s.db.Model(record).Updates(map[string]any{"status": model.KYCRejected, "reject_reason": "支付宝实名认证未通过"}).Error; err != nil {
			return nil, "", err
		}
		record.Status = model.KYCRejected
		record.RejectReason = "支付宝实名认证未通过"
	}
	return record, passed, nil
}

// SubmitRequest 是实名提交入参。
type SubmitRequest struct {
	RealName string
	IDNumber string
	Front    Upload
	Back     Upload
}

// Submit 提交或重新提交实名认证。
//
// 一人一条记录：重新提交是覆盖而非新增，旧照片在覆盖成功后删除。已通过的
// 记录不允许再改 —— 否则「审核通过」这件事就没有意义了。
func (s *KYCService) Submit(userID uint, req SubmitRequest) (*model.Verification, error) {
	name, err := ValidateRealName(req.RealName)
	if err != nil {
		return nil, err
	}
	idNumber, err := ValidateIDNumber(req.IDNumber)
	if err != nil {
		return nil, err
	}

	existing, err := s.find(userID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		switch existing.Status {
		case model.KYCApproved:
			return nil, ErrConflict("已通过实名认证，如需变更请联系客服")
		case model.KYCPending:
			return nil, ErrConflict("已有认证申请正在审核中，请耐心等待")
		}
	}

	// 同一号码被别人认证过就拒绝：只拦 approved，被驳回的记录不该占着号码。
	var taken int64
	err = s.db.Model(&model.Verification{}).
		Where("id_number = ? AND status = ? AND user_id <> ?", idNumber, model.KYCApproved, userID).
		Count(&taken).Error
	if err != nil {
		return nil, err
	}
	if taken > 0 {
		return nil, ErrConflict("该身份证号已被其他账号认证")
	}

	frontPath, err := s.savePhoto(req.Front, "人像面")
	if err != nil {
		return nil, err
	}
	backPath, err := s.savePhoto(req.Back, "国徽面")
	if err != nil {
		s.store.Remove(frontPath)
		return nil, err
	}

	now := time.Now().UTC()
	record := model.Verification{
		UserID:      userID,
		RealName:    name,
		IDNumber:    idNumber,
		FrontPath:   frontPath,
		BackPath:    backPath,
		Status:      model.KYCPending,
		SubmittedAt: now,
	}
	if existing != nil {
		record.ID = existing.ID
		record.CreatedAt = existing.CreatedAt
	}
	if err := s.db.Save(&record).Error; err != nil {
		s.store.RemoveAll([]string{frontPath, backPath})
		return nil, err
	}
	// 新照片已经入库，旧的没人引用了才删 —— 顺序颠倒就可能删掉仍在用的文件。
	if existing != nil {
		s.store.RemoveAll([]string{existing.FrontPath, existing.BackPath})
	}
	return &record, nil
}

// savePhoto 落盘一张证件照，并按内容嗅探结果卡类型。
func (s *KYCService) savePhoto(file Upload, label string) (string, error) {
	if file.Open == nil {
		return "", ErrBadRequest("请上传身份证%s照片", label)
	}
	if file.Size > MaxPhotoBytes {
		return "", ErrBadRequest("%s照片超过 %d MiB", label, MaxPhotoBytes>>20)
	}
	reader, err := file.Open()
	if err != nil {
		return "", ErrBadRequest("无法读取%s照片", label)
	}
	defer reader.Close()

	path, _, mime, err := s.store.Save(kycCategory, reader, MaxPhotoBytes)
	if err != nil {
		if errors.Is(err, storage.ErrTooLarge) {
			return "", ErrBadRequest("%s照片超过 %d MiB", label, MaxPhotoBytes>>20)
		}
		if errors.Is(err, storage.ErrEmpty) {
			return "", ErrBadRequest("%s照片内容为空", label)
		}
		return "", err
	}
	if !allowedPhotoMimes[mime] {
		s.store.Remove(path)
		return "", ErrBadRequest("%s照片仅支持 JPG、PNG 或 WebP 格式", label)
	}
	return path, nil
}

// Mine 返回用户自己的实名记录，身份证号已打码。未提交过时返回 nil。
func (s *KYCService) Mine(userID uint) (*model.Verification, error) {
	record, err := s.find(userID)
	if err != nil || record == nil {
		return nil, err
	}
	// 打码在 service 层做而不是交给前端：完整号码根本不该离开服务端。
	record.IDNumber = model.MaskIDNumber(record.IDNumber)
	return record, nil
}

// IsApproved 报告用户是否已通过实名认证。创建 API Key 的唯一闸门。
func (s *KYCService) IsApproved(userID uint) (bool, error) {
	var count int64
	err := s.db.Model(&model.Verification{}).
		Where("user_id = ? AND status = ?", userID, model.KYCApproved).Count(&count).Error
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// Photo 打开用户自己的证件照。
func (s *KYCService) Photo(userID uint, side string) (*os.File, string, error) {
	record, err := s.find(userID)
	if err != nil {
		return nil, "", err
	}
	if record == nil {
		return nil, "", ErrNotFound("尚未提交实名认证")
	}
	return s.openPhoto(record, side)
}

// AdminPhoto 打开指定记录的证件照，供管理员审核比对。
func (s *KYCService) AdminPhoto(id uint, side string) (*os.File, string, error) {
	record, err := s.Get(id)
	if err != nil {
		return nil, "", err
	}
	return s.openPhoto(record, side)
}

// openPhoto 按 side 取出对应的照片文件与 MIME。
func (s *KYCService) openPhoto(record *model.Verification, side string) (*os.File, string, error) {
	var path string
	switch side {
	case SideFront:
		path = record.FrontPath
	case SideBack:
		path = record.BackPath
	default:
		return nil, "", ErrBadRequest("照片类型只能是 front 或 back")
	}
	file, err := s.store.Open(path)
	if err != nil {
		return nil, "", ErrNotFound("照片不存在")
	}
	mime, err := sniffMime(file)
	if err != nil {
		file.Close()
		return nil, "", err
	}
	// 再嗅探一次并复核白名单：入库时校验过，但库里的值可能被别处写坏，
	// 而这里是要内联展示的，放错类型就是同源脚本。
	if !allowedPhotoMimes[mime] {
		file.Close()
		return nil, "", ErrNotFound("照片不存在")
	}
	return file, mime, nil
}

// List 分页返回实名记录，可按状态过滤。供管理端使用。
func (s *KYCService) List(status string, offset, limit int) ([]model.Verification, int64, error) {
	query := s.db.Model(&model.Verification{})
	if status != "" {
		if !model.ValidKYCStatus(status) {
			return nil, 0, ErrBadRequest("无效的认证状态")
		}
		query = query.Where("status = ?", status)
	}

	var total int64
	if err := query.Session(&gorm.Session{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var items []model.Verification
	// 待审核的排在最前，其次按提交时间倒序：管理员打开页面就是来清队列的。
	err := query.Session(&gorm.Session{}).
		Order("CASE WHEN status = 'pending' THEN 0 ELSE 1 END, submitted_at DESC").
		Offset(offset).Limit(limit).Find(&items).Error
	if err != nil {
		return nil, 0, err
	}
	for i := range items {
		// 列表里也打码：审核要看完整号码，但那是详情接口的事。
		items[i].IDNumber = model.MaskIDNumber(items[i].IDNumber)
	}
	if len(items) > 0 {
		if err := s.attachUsernames(items); err != nil {
			return nil, 0, err
		}
	}
	return items, total, nil
}

// attachUsernames 一次查库补齐提交人用户名。
func (s *KYCService) attachUsernames(items []model.Verification) error {
	ids := make([]uint, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.UserID)
	}
	var users []model.User
	if err := s.db.Select("id", "username").Where("id IN ?", ids).Find(&users).Error; err != nil {
		return err
	}
	names := make(map[uint]string, len(users))
	for _, u := range users {
		names[u.ID] = u.Username
	}
	for i := range items {
		items[i].Username = names[items[i].UserID]
	}
	return nil
}

// Get 读取实名记录详情，身份证号完整。仅供管理端审核使用。
func (s *KYCService) Get(id uint) (*model.Verification, error) {
	var record model.Verification
	if err := s.db.First(&record, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound("认证记录不存在")
		}
		return nil, err
	}
	var user model.User
	if err := s.db.Select("id", "username").First(&user, record.UserID).Error; err == nil {
		record.Username = user.Username
	}
	return &record, nil
}

// Review 审核实名记录。approved 为假时 reason 必填。
func (s *KYCService) Review(id, reviewerID uint, approved bool, reason string) (*model.Verification, error) {
	status := model.KYCApproved
	if !approved {
		status = model.KYCRejected
		reason = strings.TrimSpace(reason)
		if reason == "" {
			return nil, ErrBadRequest("请填写驳回原因")
		}
		if utf8.RuneCountInString(reason) > 255 {
			return nil, ErrBadRequest("驳回原因不能超过 255 个字符")
		}
	} else {
		reason = ""
	}

	var record model.Verification
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&record, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound("认证记录不存在")
			}
			return err
		}
		if record.Status != model.KYCPending {
			return ErrConflict("该记录已审核过")
		}

		now := time.Now().UTC()
		// 通过时若号码已被别人占用，说明两份申请并发提交，此处兜住。
		if approved {
			var taken int64
			err := tx.Model(&model.Verification{}).
				Where("id_number = ? AND status = ? AND user_id <> ?",
					record.IDNumber, model.KYCApproved, record.UserID).
				Count(&taken).Error
			if err != nil {
				return err
			}
			if taken > 0 {
				return ErrConflict("该身份证号已被其他账号认证")
			}
		}

		updates := map[string]any{
			"status":        status,
			"reject_reason": reason,
			"reviewed_by":   reviewerID,
			"reviewed_at":   now,
		}
		if err := tx.Model(&record).Updates(updates).Error; err != nil {
			return err
		}
		record.Status = status
		record.RejectReason = reason
		record.ReviewedBy = reviewerID
		record.ReviewedAt = &now
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &record, nil
}

// UserPhotoPaths 返回用户证件照的磁盘路径，供删号时清理文件。
func (s *KYCService) UserPhotoPaths(tx *gorm.DB, userID uint) ([]string, error) {
	var records []model.Verification
	if err := tx.Where("user_id = ?", userID).Find(&records).Error; err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(records)*2)
	for _, r := range records {
		paths = append(paths, r.FrontPath, r.BackPath)
	}
	return paths, nil
}

// find 读取用户的实名记录，不存在时返回 (nil, nil)。
func (s *KYCService) find(userID uint) (*model.Verification, error) {
	var record model.Verification
	err := s.db.First(&record, "user_id = ?", userID).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &record, nil
}

// sniffMime 读取文件开头判断 MIME，并把读取位置复位到开头。
func sniffMime(file *os.File) (string, error) {
	head := make([]byte, 512)
	n, err := file.Read(head)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	return http.DetectContentType(head[:n]), nil
}
