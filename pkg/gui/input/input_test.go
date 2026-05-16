package input

import (
	"reflect"
	"testing"
)

type fakeInjector struct {
	calls []string
}

func (f *fakeInjector) KeyDown(key string) error {
	f.calls = append(f.calls, "down "+key)
	return nil
}

func (f *fakeInjector) KeyUp(key string) error {
	f.calls = append(f.calls, "up "+key)
	return nil
}

func (f *fakeInjector) KeyPress(key string) error {
	f.calls = append(f.calls, "press "+key)
	return nil
}

func TestKeyboardUsesCodeForShiftedPunctuation(t *testing.T) {
	injector := &fakeInjector{}
	keyboard := NewKeyboard(injector, nil, false)

	err := keyboard.Handle(BrowserKeyEvent{
		EventType: EventKeyDown,
		Key:       "_",
		Code:      "Minus",
		ShiftKey:  true,
	})
	if err != nil {
		t.Fatal(err)
	}

	assertCalls(t, injector.calls, []string{"press shift+minus"})
}

func TestKeyboardUsesHeldModifierStateForPunctuation(t *testing.T) {
	injector := &fakeInjector{}
	keyboard := NewKeyboard(injector, nil, false)

	events := []BrowserKeyEvent{
		{EventType: EventKeyDown, Key: "Shift", Code: "ShiftLeft", ShiftKey: true},
		{EventType: EventKeyDown, Key: "?", Code: "Slash", ShiftKey: true},
	}
	for _, event := range events {
		if err := keyboard.Handle(event); err != nil {
			t.Fatal(err)
		}
	}

	assertCalls(t, injector.calls, []string{"down Shift_L", "press slash"})
}

func TestKeyboardDoesNotSynthesizeControlFromModifierBoolean(t *testing.T) {
	injector := &fakeInjector{}
	keyboard := NewKeyboard(injector, nil, false)

	err := keyboard.Handle(BrowserKeyEvent{
		EventType: EventKeyDown,
		Key:       "c",
		Code:      "KeyC",
		CtrlKey:   true,
	})
	if err != nil {
		t.Fatal(err)
	}

	assertCalls(t, injector.calls, []string{"press c"})
}

func TestKeyboardUsesTrackedControlModifier(t *testing.T) {
	injector := &fakeInjector{}
	keyboard := NewKeyboard(injector, nil, false)

	events := []BrowserKeyEvent{
		{EventType: EventKeyDown, Key: "Control", Code: "ControlLeft", CtrlKey: true},
		{EventType: EventKeyDown, Key: "c", Code: "KeyC", CtrlKey: true},
		{EventType: EventKeyUp, Key: "Control", Code: "ControlLeft"},
	}
	for _, event := range events {
		if err := keyboard.Handle(event); err != nil {
			t.Fatal(err)
		}
	}

	assertCalls(t, injector.calls, []string{"down Control_L", "press c", "up Control_L"})
}

func TestKeyboardExplicitPunctuationCodes(t *testing.T) {
	tests := []struct {
		code string
		want string
	}{
		{code: "Minus", want: "minus"},
		{code: "Equal", want: "equal"},
		{code: "BracketLeft", want: "bracketleft"},
		{code: "BracketRight", want: "bracketright"},
		{code: "Backslash", want: "backslash"},
		{code: "Semicolon", want: "semicolon"},
		{code: "Quote", want: "apostrophe"},
		{code: "Comma", want: "comma"},
		{code: "Period", want: "period"},
		{code: "Slash", want: "slash"},
		{code: "Backquote", want: "grave"},
		{code: "IntlBackslash", want: "backslash"},
	}

	for _, test := range tests {
		t.Run(test.code, func(t *testing.T) {
			injector := &fakeInjector{}
			keyboard := NewKeyboard(injector, nil, false)
			if err := keyboard.Handle(BrowserKeyEvent{EventType: EventKeyDown, Code: test.code}); err != nil {
				t.Fatal(err)
			}
			assertCalls(t, injector.calls, []string{"press " + test.want})
		})
	}
}

func TestKeyboardModifierDownUp(t *testing.T) {
	injector := &fakeInjector{}
	keyboard := NewKeyboard(injector, nil, false)

	events := []BrowserKeyEvent{
		{EventType: EventKeyDown, Key: "Shift", Code: "ShiftRight", ShiftKey: true},
		{EventType: EventKeyUp, Key: "Shift", Code: "ShiftRight"},
	}
	for _, event := range events {
		if err := keyboard.Handle(event); err != nil {
			t.Fatal(err)
		}
	}

	assertCalls(t, injector.calls, []string{"down Shift_R", "up Shift_R"})
}

func TestKeyboardRepeatAndDuplicateKeydown(t *testing.T) {
	injector := &fakeInjector{}
	keyboard := NewKeyboard(injector, nil, false)

	events := []BrowserKeyEvent{
		{EventType: EventKeyDown, Key: "a", Code: "KeyA"},
		{EventType: EventKeyDown, Key: "a", Code: "KeyA"},
		{EventType: EventKeyDown, Key: "a", Code: "KeyA", Repeat: true},
		{EventType: EventKeyUp, Key: "a", Code: "KeyA"},
	}
	for _, event := range events {
		if err := keyboard.Handle(event); err != nil {
			t.Fatal(err)
		}
	}

	assertCalls(t, injector.calls, []string{"press a", "press a"})
}

func TestKeyboardReleaseAllReleasesPressedModifiers(t *testing.T) {
	injector := &fakeInjector{}
	keyboard := NewKeyboard(injector, nil, false)

	events := []BrowserKeyEvent{
		{EventType: EventKeyDown, Key: "Shift", Code: "ShiftLeft", ShiftKey: true},
		{EventType: EventKeyDown, Key: "a", Code: "KeyA", ShiftKey: true},
	}
	for _, event := range events {
		if err := keyboard.Handle(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := keyboard.ReleaseAll(); err != nil {
		t.Fatal(err)
	}

	assertCalls(t, injector.calls, []string{"down Shift_L", "press a", "up Shift_L"})
}

func assertCalls(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("calls mismatch\ngot:  %#v\nwant: %#v", got, want)
	}
}
