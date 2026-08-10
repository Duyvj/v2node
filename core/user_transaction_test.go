package core

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/xtls/xray-core/common/protocol"
)

type fakeUserManager struct {
	users  map[string]*protocol.MemoryUser
	failAt string
}

func (m *fakeUserManager) AddUser(_ context.Context, user *protocol.MemoryUser) error {
	if user.Email == m.failAt {
		return errors.New("injected add failure")
	}
	if _, exists := m.users[user.Email]; exists {
		return errors.New("duplicate")
	}
	m.users[user.Email] = user
	return nil
}

func (m *fakeUserManager) RemoveUser(_ context.Context, email string) error {
	delete(m.users, email)
	return nil
}

func (m *fakeUserManager) GetUser(_ context.Context, email string) *protocol.MemoryUser {
	return m.users[email]
}

func (m *fakeUserManager) GetUsers(context.Context) []*protocol.MemoryUser {
	users := make([]*protocol.MemoryUser, 0, len(m.users))
	for _, user := range m.users {
		users = append(users, user)
	}
	return users
}

func (m *fakeUserManager) GetUsersCount(context.Context) int64 { return int64(len(m.users)) }

func TestAddMemoryUsersRollsBackEveryPartialBatch(t *testing.T) {
	for round := 0; round < 1000; round++ {
		manager := &fakeUserManager{users: make(map[string]*protocol.MemoryUser), failAt: "bad"}
		users := []*protocol.MemoryUser{
			{Email: fmt.Sprintf("good-%d", round)},
			{Email: "bad"},
		}
		emails := []string{users[0].Email, users[1].Email}
		if err := addMemoryUsers(manager, users, emails); err == nil {
			t.Fatal("injected failure was ignored")
		}
		if len(manager.users) != 0 {
			t.Fatalf("round %d retained ghost users: %#v", round, manager.users)
		}
	}
}
