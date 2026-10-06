package service
import (
 "context"
 "testing"
 "time"
 "github.com/SakuraOpenSource/levis/internal/model"
)
func TestAutoRenewFailureNotificationDedupe(t *testing.T) {
 db:=newTestDB(t); u:=seedUser(t,db,"poorrenew",0); svc:=seedService(t,db,u.ID,"renew",1000)
 expiry:=time.Now().UTC().Add(time.Hour); db.Model(svc).Updates(map[string]any{"auto_renew":true,"expires_at":expiry})
 lc:=newLifecycleServiceForTest(db,nil,func()bool{return false}); lc.autoRenew(context.Background());lc.autoRenew(context.Background())
 var rows []model.RenewalEvent; db.Where("kind = ?","failed").Find(&rows)
 if len(rows)!=1 {t.Fatalf("failure notification ledger count=%d",len(rows))}
 if balanceOf(t,db,u.ID)!=0 {t.Fatal("failed renewal touched wallet")}
 db.Model(&model.User{}).Where("id = ?",u.ID).Update("balance_cents",2000)
 lc.autoRenew(context.Background());lc.autoRenew(context.Background())
 if balanceOf(t,db,u.ID)!=1000 { t.Fatal("failure ledger blocked recovery or duplicated payment") }
}
