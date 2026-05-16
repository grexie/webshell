package input

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/grexie/webshell/v2/pkg/process"
)

const (
	EventKeyDown = "keydown"
	EventKeyUp   = "keyup"
)

type Modifiers struct {
	Alt   bool `json:"alt"`
	Ctrl  bool `json:"ctrl"`
	Meta  bool `json:"meta"`
	Shift bool `json:"shift"`
}

type BrowserKeyEvent struct {
	EventType string
	Key       string
	Code      string
	Location  int
	Repeat    bool
	ShiftKey  bool
	CtrlKey   bool
	AltKey    bool
	MetaKey   bool
}

type KeyInjector interface {
	KeyDown(key string) error
	KeyUp(key string) error
	KeyPress(key string) error
}

type XDoToolInjector struct {
	Display string
	RunAs   process.User
}

func (i XDoToolInjector) KeyDown(key string) error {
	return i.run("keydown", key)
}

func (i XDoToolInjector) KeyUp(key string) error {
	return i.run("keyup", key)
}

func (i XDoToolInjector) KeyPress(key string) error {
	return i.run("key", key)
}

func (i XDoToolInjector) run(args ...string) error {
	cmd := exec.Command("xdotool", args...)
	cmd.Env = envWithDisplay(i.Display)
	i.RunAs.Apply(cmd, cmd.Env)
	if err := cmd.Run(); err != nil {
		return commandError("xdotool "+strings.Join(args, " "), err)
	}
	return nil
}

type Keyboard struct {
	mu       sync.Mutex
	injector KeyInjector
	logger   *slog.Logger
	debug    bool
	pressed  map[string]pressedKey
}

type pressedKey struct {
	key          string
	injectedDown bool
}

func NewKeyboard(injector KeyInjector, logger *slog.Logger, debug bool) *Keyboard {
	if logger == nil {
		logger = slog.Default()
	}
	return &Keyboard{
		injector: injector,
		logger:   logger,
		debug:    debug,
		pressed:  make(map[string]pressedKey),
	}
}

func (k *Keyboard) Handle(event BrowserKeyEvent) error {
	if event.Code == "" && event.Key == "" {
		return nil
	}

	translated := translate(event)
	if translated.key == "" {
		k.debugEvent(event, "ignored", "")
		return nil
	}

	k.mu.Lock()
	defer k.mu.Unlock()

	switch event.EventType {
	case EventKeyDown:
		return k.keyDownLocked(event, translated)
	case EventKeyUp:
		return k.keyUpLocked(event, translated)
	default:
		return nil
	}
}

func (k *Keyboard) ReleaseAll() error {
	k.mu.Lock()
	defer k.mu.Unlock()

	var errs []error
	for code, pressed := range k.pressed {
		if pressed.injectedDown {
			if err := k.injector.KeyUp(pressed.key); err != nil {
				errs = append(errs, err)
			}
		}
		delete(k.pressed, code)
	}

	if len(errs) > 0 {
		return errorsJoin(errs)
	}
	return nil
}

func (k *Keyboard) keyDownLocked(event BrowserKeyEvent, translated translatedKey) error {
	code := keyStateID(event)
	if previous, ok := k.pressed[code]; ok && !event.Repeat {
		k.debugEvent(event, "duplicate", previous.key)
		return nil
	}

	if translated.modifier {
		if _, ok := k.pressed[code]; ok {
			k.debugEvent(event, "keydown duplicate modifier", translated.key)
			return nil
		}
		if err := k.injector.KeyDown(translated.key); err != nil {
			return err
		}
		k.pressed[code] = pressedKey{key: translated.key, injectedDown: true}
		k.debugEvent(event, "keydown", translated.key)
		return nil
	}

	combo := k.comboForLocked(event, translated.key)
	if err := k.injector.KeyPress(combo); err != nil {
		return err
	}
	k.pressed[code] = pressedKey{key: translated.key}
	k.debugEvent(event, "keypress", combo)
	return nil
}

func (k *Keyboard) keyUpLocked(event BrowserKeyEvent, translated translatedKey) error {
	code := keyStateID(event)
	pressed, ok := k.pressed[code]
	if !ok {
		k.debugEvent(event, "keyup missing", translated.key)
		return nil
	}
	delete(k.pressed, code)

	if !pressed.injectedDown {
		k.debugEvent(event, "keyup tracked", translated.key)
		return nil
	}
	if err := k.injector.KeyUp(pressed.key); err != nil {
		return err
	}
	k.debugEvent(event, "keyup", pressed.key)
	return nil
}

func (k *Keyboard) debugEvent(event BrowserKeyEvent, action, translated string) {
	if !k.debug {
		return
	}
	k.logger.Info(
		"gui key event",
		"eventType", event.EventType,
		"code", event.Code,
		"key", event.Key,
		"location", event.Location,
		"repeat", event.Repeat,
		"shift", event.ShiftKey,
		"ctrl", event.CtrlKey,
		"alt", event.AltKey,
		"meta", event.MetaKey,
		"action", action,
		"translated", translated,
	)
}

type translatedKey struct {
	key      string
	modifier bool
}

func translate(event BrowserKeyEvent) translatedKey {
	if key, ok := keyByCode(event.Code, event.Location); ok {
		return key
	}
	if key, ok := keyByValue(event.Key, event.Code); ok {
		return key
	}
	return translatedKey{}
}

func keyByCode(code string, location int) (translatedKey, bool) {
	// KeyboardEvent.code is the physical key position. Safari/macOS and iPad
	// can report punctuation in KeyboardEvent.key inconsistently, especially
	// with Shift held, so GUI sessions intentionally prefer code and apply the
	// browser modifier state later.
	if key, ok := punctuationByCode[code]; ok {
		return translatedKey{key: key}, true
	}
	if key, ok := navigationByCode[code]; ok {
		return translatedKey{key: key}, true
	}
	if key, ok := modifierByCode[code]; ok {
		return translatedKey{key: key, modifier: true}, true
	}
	if strings.HasPrefix(code, "Key") && len(code) == 4 {
		return translatedKey{key: strings.ToLower(strings.TrimPrefix(code, "Key"))}, true
	}
	if strings.HasPrefix(code, "Digit") && len(code) == 6 {
		return translatedKey{key: strings.TrimPrefix(code, "Digit")}, true
	}
	if strings.HasPrefix(code, "F") {
		if number, err := strconv.Atoi(strings.TrimPrefix(code, "F")); err == nil && number >= 1 && number <= 24 {
			return translatedKey{key: code}, true
		}
	}
	if strings.HasPrefix(code, "Numpad") {
		if key, ok := numpadByCode[code]; ok {
			return translatedKey{key: key}, true
		}
	}
	if code == "Space" {
		return translatedKey{key: "space"}, true
	}
	return translatedKey{}, false
}

func keyByValue(key, code string) (translatedKey, bool) {
	switch key {
	case "Backspace":
		return translatedKey{key: "BackSpace"}, true
	case "Tab":
		return translatedKey{key: "Tab"}, true
	case "Enter":
		return translatedKey{key: "Return"}, true
	case "Escape":
		return translatedKey{key: "Escape"}, true
	case " ":
		return translatedKey{key: "space"}, true
	case "ArrowLeft":
		return translatedKey{key: "Left"}, true
	case "ArrowRight":
		return translatedKey{key: "Right"}, true
	case "ArrowUp":
		return translatedKey{key: "Up"}, true
	case "ArrowDown":
		return translatedKey{key: "Down"}, true
	case "Shift":
		if code == "ShiftRight" {
			return translatedKey{key: "Shift_R", modifier: true}, true
		}
		return translatedKey{key: "Shift_L", modifier: true}, true
	case "Control":
		if code == "ControlRight" {
			return translatedKey{key: "Control_R", modifier: true}, true
		}
		return translatedKey{key: "Control_L", modifier: true}, true
	case "Alt":
		if code == "AltRight" {
			return translatedKey{key: "Alt_R", modifier: true}, true
		}
		return translatedKey{key: "Alt_L", modifier: true}, true
	case "Meta":
		if code == "MetaRight" {
			return translatedKey{key: "Super_R", modifier: true}, true
		}
		return translatedKey{key: "Super_L", modifier: true}, true
	}

	runes := []rune(key)
	if len(runes) == 1 {
		if keyName, ok := printableFallbacks[key]; ok {
			return translatedKey{key: keyName}, true
		}
		return translatedKey{key: strings.ToLower(key)}, true
	}
	return translatedKey{}, false
}

var punctuationByCode = map[string]string{
	"Minus":         "minus",
	"Equal":         "equal",
	"BracketLeft":   "bracketleft",
	"BracketRight":  "bracketright",
	"Backslash":     "backslash",
	"Semicolon":     "semicolon",
	"Quote":         "apostrophe",
	"Comma":         "comma",
	"Period":        "period",
	"Slash":         "slash",
	"Backquote":     "grave",
	"IntlBackslash": "backslash",
}

var navigationByCode = map[string]string{
	"Backspace":  "BackSpace",
	"Tab":        "Tab",
	"Enter":      "Return",
	"Escape":     "Escape",
	"ArrowLeft":  "Left",
	"ArrowRight": "Right",
	"ArrowUp":    "Up",
	"ArrowDown":  "Down",
	"Delete":     "Delete",
	"Insert":     "Insert",
	"Home":       "Home",
	"End":        "End",
	"PageUp":     "Page_Up",
	"PageDown":   "Page_Down",
	"CapsLock":   "Caps_Lock",
}

var modifierByCode = map[string]string{
	"ShiftLeft":    "Shift_L",
	"ShiftRight":   "Shift_R",
	"ControlLeft":  "Control_L",
	"ControlRight": "Control_R",
	"AltLeft":      "Alt_L",
	"AltRight":     "Alt_R",
	"MetaLeft":     "Super_L",
	"MetaRight":    "Super_R",
}

var numpadByCode = map[string]string{
	"Numpad0":        "KP_0",
	"Numpad1":        "KP_1",
	"Numpad2":        "KP_2",
	"Numpad3":        "KP_3",
	"Numpad4":        "KP_4",
	"Numpad5":        "KP_5",
	"Numpad6":        "KP_6",
	"Numpad7":        "KP_7",
	"Numpad8":        "KP_8",
	"Numpad9":        "KP_9",
	"NumpadAdd":      "KP_Add",
	"NumpadSubtract": "KP_Subtract",
	"NumpadMultiply": "KP_Multiply",
	"NumpadDivide":   "KP_Divide",
	"NumpadDecimal":  "KP_Decimal",
	"NumpadEnter":    "KP_Enter",
}

var printableFallbacks = map[string]string{
	"+":  "plus",
	"-":  "minus",
	"_":  "underscore",
	"=":  "equal",
	"/":  "slash",
	"?":  "question",
	"`":  "grave",
	"~":  "asciitilde",
	"\\": "backslash",
	"|":  "bar",
	"[":  "bracketleft",
	"]":  "bracketright",
	"{":  "braceleft",
	"}":  "braceright",
	";":  "semicolon",
	":":  "colon",
	"'":  "apostrophe",
	"\"": "quotedbl",
	",":  "comma",
	".":  "period",
	"<":  "less",
	">":  "greater",
}

func (k *Keyboard) comboForLocked(event BrowserKeyEvent, key string) string {
	parts := make([]string, 0, 5)
	if event.ShiftKey && !k.hasPressedModifierLocked("ShiftLeft", "ShiftRight") {
		parts = append(parts, "shift")
	}
	parts = append(parts, key)
	return strings.Join(parts, "+")
}

func (k *Keyboard) hasPressedModifierLocked(codes ...string) bool {
	for _, code := range codes {
		pressed, ok := k.pressed[code]
		if ok && pressed.injectedDown {
			return true
		}
	}
	return false
}

func keyStateID(event BrowserKeyEvent) string {
	if event.Code != "" {
		return event.Code
	}
	return fmt.Sprintf("key:%s:%d", event.Key, event.Location)
}

func envWithDisplay(display string) []string {
	env := os.Environ()
	for i, value := range env {
		if strings.HasPrefix(value, "DISPLAY=") {
			env[i] = "DISPLAY=" + display
			return env
		}
	}
	return append(env, "DISPLAY="+display)
}

func commandError(action string, err error) error {
	if err == nil {
		return nil
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		stderr := strings.TrimSpace(string(exitErr.Stderr))
		if stderr != "" {
			return fmt.Errorf("%s: %w: %s", action, err, stderr)
		}
	}
	return fmt.Errorf("%s: %w", action, err)
}

func errorsJoin(errs []error) error {
	messages := make([]string, 0, len(errs))
	for _, err := range errs {
		if err != nil {
			messages = append(messages, err.Error())
		}
	}
	if len(messages) == 0 {
		return nil
	}
	return errors.New(strings.Join(messages, "; "))
}
