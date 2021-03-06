package typesjson

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/pkg/errors"
)

type Progress struct {
	Data  []ProgressItem `json:"data"`
	Error string         `json:"error,omitempty"`
}

type ProgressItem struct {
	Announcement             string        `json:"announcement"`
	ExpireDate               sql.NullTime  `json:"expireDate"`
	GroupId                  int           `json:"groupId"`
	InProgress               bool          `json:"inProgress"`
	MaxQty                   int           `json:"maxQty"`
	MaxTotalCost             int           `json:"maxTotalCost"`
	OrderHashId              string        `json:"orderHashId"`
	Originator               string        `json:"originator"`
	PasswordLocked           bool          `json:"passwordLocked"`
	RemainSecondBeforeExpire time.Duration `json:"remainSecondBeforeExpire"`
	ShopName                 string        `json:"shopName"`
	Size                     int           `json:"size"`
	Total                    int           `json:"total"`
	Unlockable               bool          `json:"unlockable"`
}

func (item *ProgressItem) IsExpiring(priorTime time.Duration) bool {
	return item.RemainSecondBeforeExpire > 0 && time.Duration(item.RemainSecondBeforeExpire) <= priorTime
}

func (item *ProgressItem) UpdateRemainSecondBeforeExpire(now time.Time) {
	if !item.ExpireDate.Valid || !item.ExpireDate.Time.After(now) {
		item.RemainSecondBeforeExpire = 0
	} else {
		item.RemainSecondBeforeExpire = item.ExpireDate.Time.Sub(now)
	}
}

func (item *ProgressItem) IsExpired() bool {
	return item.RemainSecondBeforeExpire <= 0
}

func (item *ProgressItem) GetPath() string {
	return fmt.Sprintf("/do/order?id=%s", url.QueryEscape(item.OrderHashId))
}

func (item *ProgressItem) UnmarshalJSON(data []byte) error {
	type Alias ProgressItem

	aux := struct {
		ExpireDate *int64 `json:"expireDate"`
		*Alias
	}{
		Alias: (*Alias)(item),
	}

	if err := json.Unmarshal(data, &aux); err != nil {
		return errors.WithStack(err)
	}

	if aux.ExpireDate == nil {
		item.ExpireDate = sql.NullTime{Time: time.Time{}, Valid: false}
	} else {
		item.ExpireDate = sql.NullTime{Time: time.UnixMilli(*aux.ExpireDate), Valid: true}
	}

	item.RemainSecondBeforeExpire *= time.Second

	return nil
}
