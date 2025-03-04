// Copyright 2015 Hajime Hoshi
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// +build darwin freebsd linux windows
// +build !android
// +build !ios

package glfw

import (
	"fmt"
	"image"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hajimehoshi/ebiten/v2/internal/devicescale"
	"github.com/hajimehoshi/ebiten/v2/internal/driver"
	"github.com/hajimehoshi/ebiten/v2/internal/glfw"
	"github.com/hajimehoshi/ebiten/v2/internal/hooks"
	"github.com/hajimehoshi/ebiten/v2/internal/thread"
)

type UserInterface struct {
	context driver.UIContext
	title   string
	window  *glfw.Window

	// windowWidth and windowHeight represents a window size.
	// The unit is device-dependent pixels.
	windowWidth  int
	windowHeight int

	running             uint32
	toChangeSize        bool
	origPosX            int
	origPosY            int
	runnableOnUnfocused bool
	vsync               bool
	iconImages          []image.Image

	// err must be accessed from the main thread.
	err error

	lastDeviceScaleFactor float64

	// These values are not changed after initialized.
	// TODO: the fullscreen size should be updated when the initial window position is changed?
	initMonitor              *glfw.Monitor
	initFullscreenWidthInDP  int
	initFullscreenHeightInDP int

	initTitle               string
	initVsync               bool
	initFullscreen          bool
	initCursorMode          driver.CursorMode
	initWindowDecorated     bool
	initWindowResizable     bool
	initWindowPositionXInDP int
	initWindowPositionYInDP int
	initWindowWidthInDP     int
	initWindowHeightInDP    int
	initWindowFloating      bool
	initWindowMaximized     bool
	initScreenTransparent   bool
	initFocused             bool

	vsyncInited bool

	input   Input
	iwindow window

	t thread.Thread
	m sync.RWMutex
}

const (
	maxInt     = int(^uint(0) >> 1)
	minInt     = -maxInt - 1
	invalidPos = minInt
)

var (
	theUI = &UserInterface{
		runnableOnUnfocused:     true,
		origPosX:                invalidPos,
		origPosY:                invalidPos,
		initVsync:               true,
		initCursorMode:          driver.CursorModeVisible,
		initWindowDecorated:     true,
		initWindowPositionXInDP: invalidPos,
		initWindowPositionYInDP: invalidPos,
		initWindowWidthInDP:     640,
		initWindowHeightInDP:    480,
		initFocused:             true,
		vsync:                   true,
	}
)

func init() {
	theUI.input.ui = theUI
	theUI.iwindow.ui = theUI
}

func Get() *UserInterface {
	return theUI
}

func init() {
	hideConsoleWindowOnWindows()
	if err := initialize(); err != nil {
		panic(err)
	}
	glfw.SetMonitorCallback(func(monitor *glfw.Monitor, event glfw.PeripheralEvent) {
		cacheMonitors()
	})
	cacheMonitors()
}

func initialize() error {
	if err := glfw.Init(); err != nil {
		return err
	}
	glfw.WindowHint(glfw.Visible, glfw.False)
	glfw.WindowHint(glfw.ClientAPI, glfw.NoAPI)

	// Create a window to set the initial monitor.
	w, err := glfw.CreateWindow(16, 16, "", nil, nil)
	if err != nil {
		return err
	}
	if w == nil {
		// This can happen on Windows Remote Desktop (#903).
		panic("glfw: glfw.CreateWindow must not return nil")
	}
	defer w.Destroy()

	m := currentMonitor(w)
	theUI.initMonitor = m
	v := m.GetVideoMode()
	scale := devicescale.GetAt(currentMonitor(w).GetPos())
	theUI.initFullscreenWidthInDP = int(fromGLFWMonitorPixel(float64(v.Width), scale))
	theUI.initFullscreenHeightInDP = int(fromGLFWMonitorPixel(float64(v.Height), scale))

	return nil
}

type cachedMonitor struct {
	m  *glfw.Monitor
	vm *glfw.VidMode
	// Pos of monitor in virtual coords
	x int
	y int
}

// monitors is the monitor list cache for desktop glfw compile targets.
// populated by 'cacheMonitors' which is called on init and every
// monitor config change event.
//
// monitors must be manipulated on the main thread.
var monitors []*cachedMonitor

func cacheMonitors() {
	monitors = nil
	ms := glfw.GetMonitors()
	for _, m := range ms {
		x, y := m.GetPos()
		monitors = append(monitors, &cachedMonitor{
			m:  m,
			vm: m.GetVideoMode(),
			x:  x,
			y:  y,
		})
	}
}

// getCachedMonitor returns a monitor for the given window x/y,
// or returns nil if monitor is not found.
//
// getCachedMonitor must be called on the main thread.
func getCachedMonitor(wx, wy int) *cachedMonitor {
	for _, m := range monitors {
		if m.x <= wx && wx < m.x+m.vm.Width && m.y <= wy && wy < m.y+m.vm.Height {
			return m
		}
	}
	return nil
}

func (u *UserInterface) isRunning() bool {
	return atomic.LoadUint32(&u.running) != 0
}

func (u *UserInterface) setRunning(running bool) {
	if running {
		atomic.StoreUint32(&u.running, 1)
	} else {
		atomic.StoreUint32(&u.running, 0)
	}
}

func (u *UserInterface) getInitTitle() string {
	u.m.RLock()
	v := u.initTitle
	u.m.RUnlock()
	return v
}

func (u *UserInterface) setInitTitle(title string) {
	u.m.RLock()
	u.initTitle = title
	u.m.RUnlock()
}

func (u *UserInterface) isInitVsyncEnabled() bool {
	u.m.RLock()
	v := u.initVsync
	u.m.RUnlock()
	return v
}

func (u *UserInterface) isInitFullscreen() bool {
	u.m.RLock()
	v := u.initFullscreen
	u.m.RUnlock()
	return v
}

func (u *UserInterface) setInitFullscreen(initFullscreen bool) {
	u.m.Lock()
	u.initFullscreen = initFullscreen
	u.m.Unlock()
}

func (u *UserInterface) getInitCursorMode() driver.CursorMode {
	u.m.RLock()
	v := u.initCursorMode
	u.m.RUnlock()
	return v
}

func (u *UserInterface) setInitCursorMode(mode driver.CursorMode) {
	u.m.Lock()
	u.initCursorMode = mode
	u.m.Unlock()
}

func (u *UserInterface) isInitWindowDecorated() bool {
	u.m.RLock()
	v := u.initWindowDecorated
	u.m.RUnlock()
	return v
}

func (u *UserInterface) setInitWindowDecorated(decorated bool) {
	u.m.Lock()
	u.initWindowDecorated = decorated
	u.m.Unlock()
}

func (u *UserInterface) isRunnableOnUnfocused() bool {
	u.m.RLock()
	v := u.runnableOnUnfocused
	u.m.RUnlock()
	return v
}

func (u *UserInterface) setRunnableOnUnfocused(runnableOnUnfocused bool) {
	u.m.Lock()
	u.runnableOnUnfocused = runnableOnUnfocused
	u.m.Unlock()
}

func (u *UserInterface) isInitWindowResizable() bool {
	u.m.RLock()
	v := u.initWindowResizable
	u.m.RUnlock()
	return v
}

func (u *UserInterface) setInitWindowResizable(resizable bool) {
	u.m.Lock()
	u.initWindowResizable = resizable
	u.m.Unlock()
}

func (u *UserInterface) isInitScreenTransparent() bool {
	u.m.RLock()
	v := u.initScreenTransparent
	u.m.RUnlock()
	return v
}

func (u *UserInterface) setInitScreenTransparent(transparent bool) {
	u.m.RLock()
	u.initScreenTransparent = transparent
	u.m.RUnlock()
}

func (u *UserInterface) getIconImages() []image.Image {
	u.m.RLock()
	i := u.iconImages
	u.m.RUnlock()
	return i
}

func (u *UserInterface) setIconImages(iconImages []image.Image) {
	u.m.Lock()
	u.iconImages = iconImages
	u.m.Unlock()
}

func (u *UserInterface) getInitWindowPosition() (int, int) {
	u.m.RLock()
	defer u.m.RUnlock()
	if u.initWindowPositionXInDP != invalidPos && u.initWindowPositionYInDP != invalidPos {
		return u.initWindowPositionXInDP, u.initWindowPositionYInDP
	}
	return invalidPos, invalidPos
}

func (u *UserInterface) setInitWindowPosition(x, y int) {
	u.m.Lock()
	defer u.m.Unlock()

	u.initWindowPositionXInDP = x
	u.initWindowPositionYInDP = y
}

func (u *UserInterface) getInitWindowSize() (int, int) {
	u.m.Lock()
	w, h := u.initWindowWidthInDP, u.initWindowHeightInDP
	u.m.Unlock()
	return w, h
}

func (u *UserInterface) setInitWindowSize(width, height int) {
	u.m.Lock()
	u.initWindowWidthInDP, u.initWindowHeightInDP = width, height
	u.m.Unlock()
}

func (u *UserInterface) isInitWindowFloating() bool {
	u.m.Lock()
	f := u.initWindowFloating
	u.m.Unlock()
	return f
}

func (u *UserInterface) setInitWindowFloating(floating bool) {
	u.m.Lock()
	u.initWindowFloating = floating
	u.m.Unlock()
}

func (u *UserInterface) isInitWindowMaximized() bool {
	u.m.Lock()
	f := u.initWindowMaximized
	u.m.Unlock()
	return f
}

func (u *UserInterface) setInitWindowMaximized(floating bool) {
	u.m.Lock()
	u.initWindowMaximized = floating
	u.m.Unlock()
}

func (u *UserInterface) isInitFocused() bool {
	u.m.Lock()
	v := u.initFocused
	u.m.Unlock()
	return v
}

func (u *UserInterface) setInitFocused(focused bool) {
	u.m.Lock()
	u.initFocused = focused
	u.m.Unlock()
}

func (u *UserInterface) ScreenSizeInFullscreen() (int, int) {
	if !u.isRunning() {
		return u.initFullscreenWidthInDP, u.initFullscreenHeightInDP
	}

	var w, h int
	_ = u.t.Call(func() error {
		v := currentMonitor(u.window).GetVideoMode()
		s := u.deviceScaleFactor()
		w = int(fromGLFWMonitorPixel(float64(v.Width), s))
		h = int(fromGLFWMonitorPixel(float64(v.Height), s))
		return nil
	})
	return w, h
}

// isFullscreen must be called from the main thread.
func (u *UserInterface) isFullscreen() bool {
	if !u.isRunning() {
		panic("glfw: isFullscreen can't be called before the main loop starts")
	}
	return u.window.GetMonitor() != nil
}

func (u *UserInterface) IsFullscreen() bool {
	if !u.isRunning() {
		return u.isInitFullscreen()
	}
	b := false
	_ = u.t.Call(func() error {
		b = u.isFullscreen()
		return nil
	})
	return b
}

func (u *UserInterface) SetFullscreen(fullscreen bool) {
	if !u.isRunning() {
		u.setInitFullscreen(fullscreen)
		return
	}

	var update bool
	_ = u.t.Call(func() error {
		update = u.isFullscreen() != fullscreen
		return nil
	})
	if !update {
		return
	}

	_ = u.t.Call(func() error {
		w, h := u.windowWidth, u.windowHeight
		u.setWindowSize(w, h, fullscreen)
		return nil
	})
}

func (u *UserInterface) IsFocused() bool {
	if !u.isRunning() {
		return false
	}

	var focused bool
	_ = u.t.Call(func() error {
		focused = u.window.GetAttrib(glfw.Focused) == glfw.True
		return nil
	})
	return focused
}

func (u *UserInterface) SetRunnableOnUnfocused(runnableOnUnfocused bool) {
	u.setRunnableOnUnfocused(runnableOnUnfocused)
}

func (u *UserInterface) IsRunnableOnUnfocused() bool {
	return u.isRunnableOnUnfocused()
}

func (u *UserInterface) SetVsyncEnabled(enabled bool) {
	if !u.isRunning() {
		// In general, m is used for locking init* values.
		// m is not used for updating vsync in setWindowSize so far, but
		// it should be OK since any goroutines can't reach here when
		// the game already starts and setWindowSize can be called.
		u.m.Lock()
		u.initVsync = enabled
		u.m.Unlock()
		return
	}
	_ = u.t.Call(func() error {
		if !u.vsyncInited {
			u.m.Lock()
			u.initVsync = enabled
			u.m.Unlock()
			return nil
		}
		u.vsync = enabled
		u.updateVsync()
		return nil
	})
}

func (u *UserInterface) IsVsyncEnabled() bool {
	if !u.isRunning() {
		return u.isInitVsyncEnabled()
	}
	var v bool
	_ = u.t.Call(func() error {
		if !u.vsyncInited {
			v = u.isInitVsyncEnabled()
			return nil
		}
		v = u.vsync
		return nil
	})
	return v
}

func (u *UserInterface) CursorMode() driver.CursorMode {
	if !u.isRunning() {
		return u.getInitCursorMode()
	}
	var v driver.CursorMode
	_ = u.t.Call(func() error {
		mode := u.window.GetInputMode(glfw.CursorMode)
		switch mode {
		case glfw.CursorNormal:
			v = driver.CursorModeVisible
		case glfw.CursorHidden:
			v = driver.CursorModeHidden
		case glfw.CursorDisabled:
			v = driver.CursorModeCaptured
		default:
			panic(fmt.Sprintf("invalid cursor mode: %d", mode))
		}
		return nil
	})
	return v
}

func (u *UserInterface) SetCursorMode(mode driver.CursorMode) {
	if !u.isRunning() {
		u.setInitCursorMode(mode)
		return
	}
	_ = u.t.Call(func() error {
		var c int
		switch mode {
		case driver.CursorModeVisible:
			c = glfw.CursorNormal
		case driver.CursorModeHidden:
			c = glfw.CursorHidden
		case driver.CursorModeCaptured:
			c = glfw.CursorDisabled
		default:
			panic(fmt.Sprintf("invalid cursor mode: %d", mode))
		}
		u.window.SetInputMode(glfw.CursorMode, c)
		return nil
	})
}

func (u *UserInterface) DeviceScaleFactor() float64 {
	if !u.isRunning() {
		return devicescale.GetAt(u.initMonitor.GetPos())
	}

	f := 0.0
	_ = u.t.Call(func() error {
		f = u.deviceScaleFactor()
		return nil
	})
	return f
}

// deviceScaleFactor must be called from the main thread.
func (u *UserInterface) deviceScaleFactor() float64 {
	// Before calling SetWindowPosition, the window's position is not reliable.
	if u.iwindow.setPositionCalled {
		// Avoid calling monitor.GetPos if we have the monitor position cached already.
		if cm := getCachedMonitor(u.window.GetPos()); cm != nil {
			return devicescale.GetAt(cm.x, cm.y)
		}
	}
	return devicescale.GetAt(currentMonitor(u.window).GetPos())
}

func init() {
	// Lock the main thread.
	runtime.LockOSThread()
}

func (u *UserInterface) RunWithoutMainLoop(context driver.UIContext) {
	panic("glfw: RunWithoutMainLoop is not implemented")
}

// createWindow creates a GLFW window.
//
// createWindow must be called from the main thread.
//
// createWindow does not set the position or size so far.
func (u *UserInterface) createWindow() error {
	if u.window != nil {
		panic("glfw: u.window must not exist at createWindow")
	}

	// As a start, create a window with temporary size to create OpenGL context thread.
	window, err := glfw.CreateWindow(16, 16, "", nil, nil)
	if err != nil {
		return err
	}
	u.window = window

	if u.Graphics().IsGL() {
		u.window.MakeContextCurrent()
	}

	u.window.SetInputMode(glfw.StickyMouseButtonsMode, glfw.True)
	u.window.SetInputMode(glfw.StickyKeysMode, glfw.True)

	mode := glfw.CursorNormal
	switch u.getInitCursorMode() {
	case driver.CursorModeHidden:
		mode = glfw.CursorHidden
	case driver.CursorModeCaptured:
		mode = glfw.CursorDisabled
	}
	u.window.SetInputMode(glfw.CursorMode, mode)
	u.window.SetTitle(u.title)
	// TODO: Set icons

	return nil
}

// unregisterWindowSetSizeCallback must be called from the main thread.
func (u *UserInterface) unregisterWindowSetSizeCallback() {
	u.window.SetSizeCallback(nil)
}

// registerWindowSetSizeCallback must be called from the main thread.
func (u *UserInterface) registerWindowSetSizeCallback() {
	u.window.SetSizeCallback(func(_ *glfw.Window, width, height int) {
		if u.window.GetAttrib(glfw.Resizable) == glfw.False {
			return
		}
		if u.isFullscreen() {
			return
		}

		if err := u.runOnAnotherThreadFromMainThread(func() error {
			var outsideWidth, outsideHeight float64
			var outsideSizeChanged bool

			_ = u.t.Call(func() error {
				if width != 0 || height != 0 {
					u.setWindowSize(width, height, u.isFullscreen())
				}

				outsideWidth, outsideHeight, outsideSizeChanged = u.updateSize()
				return nil
			})
			if outsideSizeChanged {
				u.context.Layout(outsideWidth, outsideHeight)
			}
			if err := u.context.ForceUpdate(); err != nil {
				return err
			}
			if u.Graphics().IsGL() {
				_ = u.t.Call(func() error {
					u.swapBuffers()
					return nil
				})
			}
			return nil
		}); err != nil {
			u.err = err
		}
	})
}

func (u *UserInterface) init() error {
	if u.Graphics().IsGL() {
		glfw.WindowHint(glfw.ClientAPI, glfw.OpenGLAPI)
		glfw.WindowHint(glfw.ContextVersionMajor, 2)
		glfw.WindowHint(glfw.ContextVersionMinor, 1)
	} else {
		glfw.WindowHint(glfw.ClientAPI, glfw.NoAPI)
	}

	decorated := glfw.False
	if u.isInitWindowDecorated() {
		decorated = glfw.True
	}
	glfw.WindowHint(glfw.Decorated, decorated)

	transparent := glfw.False
	if u.isInitScreenTransparent() {
		transparent = glfw.True
	}
	glfw.WindowHint(glfw.TransparentFramebuffer, transparent)
	u.Graphics().SetTransparent(u.isInitScreenTransparent())

	resizable := glfw.False
	if u.isInitWindowResizable() {
		resizable = glfw.True
	}
	glfw.WindowHint(glfw.Resizable, resizable)

	floating := glfw.False
	if u.isInitWindowFloating() {
		floating = glfw.True
	}
	glfw.WindowHint(glfw.Floating, floating)

	focused := glfw.False
	if u.isInitFocused() {
		focused = glfw.True
	}
	glfw.WindowHint(glfw.FocusOnShow, focused)

	// Set the window visible explicitly or the application freezes on Wayland (#974).
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		glfw.WindowHint(glfw.Visible, glfw.True)
	}

	if err := u.createWindow(); err != nil {
		return err
	}
	u.registerWindowSetSizeCallback()

	setPosition := func() {
		u.iwindow.setPosition(u.getInitWindowPosition())
	}
	setSize := func() {
		ww, wh := u.getInitWindowSize()
		ww = int(u.toGLFWPixel(float64(ww)))
		wh = int(u.toGLFWPixel(float64(wh)))
		u.setWindowSize(ww, wh, u.isFullscreen())
	}

	// Set the window size and the window position in this order on Linux or other UNIX using X (#1118),
	// but this should be inverted on Windows. This is very tricky, but there is no obvious way to solve
	// this. This doesn't matter on macOS.
	if runtime.GOOS == "windows" {
		setPosition()
		setSize()
	} else {
		setSize()
		setPosition()
	}

	// Maximizing a window requires a proper size and position. Call Maximize here (#1117).
	if u.isInitWindowMaximized() {
		u.window.Maximize()
	}

	u.title = u.getInitTitle()
	u.window.SetTitle(u.title)
	u.window.Show()

	if g, ok := u.Graphics().(interface{ SetWindow(uintptr) }); ok {
		g.SetWindow(u.nativeWindow())
	}

	return nil
}

func (u *UserInterface) updateSize() (float64, float64, bool) {
	ww, wh := u.windowWidth, u.windowHeight
	u.setWindowSize(ww, wh, u.isFullscreen())

	if !u.toChangeSize {
		return 0, 0, false
	}
	u.toChangeSize = false

	var w, h float64
	if u.isFullscreen() {
		v := currentMonitor(u.window).GetVideoMode()
		ww, wh := v.Width, v.Height
		s := u.deviceScaleFactor()
		w = fromGLFWMonitorPixel(float64(ww), s)
		h = fromGLFWMonitorPixel(float64(wh), s)
	} else {
		// Instead of u.windowWidth and u.windowHeight, use the actual window size here.
		// On Windows, the specified size at SetSize and the actual window size might not
		// match (#1163).
		ww, wh := u.window.GetSize()
		w = u.fromGLFWPixel(float64(ww))
		h = u.fromGLFWPixel(float64(wh))
	}
	// On Linux/UNIX, further adjusting is required (#1307).
	w = u.toFramebufferPixel(w)
	h = u.toFramebufferPixel(h)

	return w, h, true
}

// update must be called from the main thread.
func (u *UserInterface) update() (float64, float64, bool, error) {
	if u.err != nil {
		return 0, 0, false, u.err
	}

	if u.window.ShouldClose() {
		return 0, 0, false, driver.RegularTermination
	}

	if u.isInitFullscreen() {
		w, h := u.window.GetSize()
		u.setWindowSize(w, h, true)
		u.setInitFullscreen(false)
	}

	// Initialize vsync after SetMonitor is called. See the comment in updateVsync.
	// Calling this inside setWindowSize didn't work (#1363).
	if !u.vsyncInited {
		u.vsync = u.isInitVsyncEnabled()
		u.updateVsync()
		u.vsyncInited = true
	}

	outsideWidth, outsideHeight, outsideSizeChanged := u.updateSize()

	// TODO: Updating the input can be skipped when clock.Update returns 0 (#1367).
	glfw.PollEvents()
	u.input.update(u.window, u.context)

	for !u.isRunnableOnUnfocused() && u.window.GetAttrib(glfw.Focused) == 0 && !u.window.ShouldClose() {
		hooks.SuspendAudio()
		// Wait for an arbitrary period to avoid busy loop.
		time.Sleep(time.Second / 60)
		glfw.PollEvents()
	}
	hooks.ResumeAudio()

	return outsideWidth, outsideHeight, outsideSizeChanged, nil
}

func (u *UserInterface) loop() error {
	defer func() {
		_ = u.t.Call(func() error {
			glfw.Terminate()
			return nil
		})
	}()

	for {
		var unfocused bool

		// On Windows, the focusing state might be always false (#987).
		// On Windows, even if a window is in another workspace, vsync seems to work.
		// Then let's assume the window is always 'focused' as a workaround.
		if runtime.GOOS != "windows" {
			unfocused = u.window.GetAttrib(glfw.Focused) == glfw.False
		}

		var t1, t2 time.Time

		if unfocused {
			t1 = time.Now()
		}

		var outsideWidth, outsideHeight float64
		var outsideSizeChanged bool
		if err := u.t.Call(func() error {
			var err error
			outsideWidth, outsideHeight, outsideSizeChanged, err = u.update()
			return err
		}); err != nil {
			return err
		}
		if outsideSizeChanged {
			u.context.Layout(outsideWidth, outsideHeight)
		}

		if err := u.context.Update(); err != nil {
			return err
		}

		if imgs := u.getIconImages(); imgs != nil {
			u.setIconImages(nil)

			// Convert the icons in the different goroutine, as (*ebiten.Image).At cannot be invoked
			// from this goroutine. At works only in between BeginFrame and EndFrame.
			go func() {
				newImgs := make([]image.Image, len(imgs))
				for i, img := range imgs {
					// TODO: If img is not *ebiten.Image, this converting is not necessary.
					// However, this package cannot refer *ebiten.Image due to the package
					// dependencies.

					b := img.Bounds()
					rgba := image.NewRGBA(b)
					for j := b.Min.Y; j < b.Max.Y; j++ {
						for i := b.Min.X; i < b.Max.X; i++ {
							rgba.Set(i, j, img.At(i, j))
						}
					}
					newImgs[i] = rgba
				}

				_ = u.t.Call(func() error {
					u.window.SetIcon(newImgs)
					return nil
				})
			}()
		}

		// swapBuffers also checks IsGL, so this condition is redundant.
		// However, (*thread).Call is not good for performance due to channels.
		// Let's avoid this whenever possible (#1367).
		if u.Graphics().IsGL() {
			_ = u.t.Call(func() error {
				u.swapBuffers()
				return nil
			})
		}

		if unfocused {
			t2 = time.Now()
		}

		// When a window is not focused, SwapBuffers might return immediately and CPU might be busy.
		// Mitigate this by sleeping (#982).
		if unfocused {
			d := t2.Sub(t1)
			const wait = time.Second / 60
			if d < wait {
				time.Sleep(wait - d)
			}
		}
	}
}

// swapBuffers must be called from the main thread.
func (u *UserInterface) swapBuffers() {
	if u.Graphics().IsGL() {
		u.window.SwapBuffers()
	}
}

// setWindowSize must be called from the main thread.
func (u *UserInterface) setWindowSize(width, height int, fullscreen bool) {
	if u.windowWidth == width && u.windowHeight == height && u.isFullscreen() == fullscreen && u.lastDeviceScaleFactor == u.deviceScaleFactor() {
		return
	}

	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}

	u.lastDeviceScaleFactor = u.deviceScaleFactor()

	// To make sure the current existing framebuffers are rendered,
	// swap buffers here before SetSize is called.
	u.swapBuffers()

	// Do not fire the callback of SetSize. This callback can be invoked by SetMonitor or SetSize.
	// ForceUpdate is called from the callback.
	// While setWindowSize can be called from Update, calling ForceUpdate inside Update is illegal (#1505).
	u.unregisterWindowSetSizeCallback()
	defer u.registerWindowSetSizeCallback()

	var windowRecreated bool

	if fullscreen {
		if u.origPosX == invalidPos || u.origPosY == invalidPos {
			u.origPosX, u.origPosY = u.window.GetPos()
		}
		m := currentMonitor(u.window)
		v := m.GetVideoMode()
		u.window.SetMonitor(m, 0, 0, v.Width, v.Height, v.RefreshRate)

		// Swapping buffer is necesary to prevent the image lag (#1004).
		// TODO: This might not work when vsync is disabled.
		if u.Graphics().IsGL() {
			glfw.PollEvents()
			u.swapBuffers()
		}
	} else {
		// On Windows, giving a too small width doesn't call a callback (#165).
		// To prevent hanging up, return asap if the width is too small.
		// 126 is an arbitrary number and I guess this is small enough.
		minWindowWidth := int(u.toGLFWPixel(126))
		if u.window.GetAttrib(glfw.Decorated) == glfw.False {
			minWindowWidth = 1
		}
		if width < minWindowWidth {
			width = minWindowWidth
		}

		if u.window.GetMonitor() != nil {
			if u.Graphics().IsGL() {
				// When OpenGL is used, swapping buffer is enough to solve the image-lag
				// issue (#1004). Rather, recreating window destroys GPU resources.
				// TODO: This might not work when vsync is disabled.
				u.window.SetMonitor(nil, 0, 0, width, height, 0)
				glfw.PollEvents()
				u.swapBuffers()
			} else {
				// Recreate the window since an image lag remains after coming back from
				// fullscreen (#1004).
				if u.window != nil {
					u.window.Destroy()
					u.window = nil
				}
				if err := u.createWindow(); err != nil {
					// TODO: This should return an error.
					panic(fmt.Sprintf("glfw: failed to recreate window: %v", err))
				}
				u.window.Show()
				windowRecreated = true
			}
		}

		if u.origPosX != invalidPos && u.origPosY != invalidPos {
			x := u.origPosX
			y := u.origPosY
			u.window.SetPos(x, y)
			// Dirty hack for macOS (#703). Rendering doesn't work correctly with one SetPos, but
			// work with two or more SetPos.
			if runtime.GOOS == "darwin" {
				u.window.SetPos(x+1, y)
				u.window.SetPos(x, y)
			}
			u.origPosX = invalidPos
			u.origPosY = invalidPos
		}

		// Set the window size after the position. The order matters.
		// In the opposite order, the window size might not be correct when going back from fullscreen with multi monitors.
		oldW, oldH := u.window.GetSize()
		newW := width
		newH := height
		if oldW != newW || oldH != newH {
			ch := make(chan struct{})
			u.window.SetFramebufferSizeCallback(func(_ *glfw.Window, _, _ int) {
				u.window.SetFramebufferSizeCallback(nil)
				close(ch)
			})
			u.window.SetSize(newW, newH)
		event:
			for {
				glfw.PollEvents()
				select {
				case <-ch:
					break event
				default:
				}
			}
		}

		// Window title might be lost on macOS after coming back from fullscreen.
		u.window.SetTitle(u.title)
	}

	// As width might be updated, update windowWidth/Height here.
	u.windowWidth = width
	u.windowHeight = height

	u.toChangeSize = true

	if windowRecreated {
		if g, ok := u.Graphics().(interface{ SetWindow(uintptr) }); ok {
			g.SetWindow(u.nativeWindow())
		}
	}
}

// updateVsync must be called on the main thread.
func (u *UserInterface) updateVsync() {
	if u.Graphics().IsGL() {
		// SwapInterval is affected by the current monitor of the window.
		// This needs to be called at least after SetMonitor.
		// Without SwapInterval after SetMonitor, vsynch doesn't work (#375).
		//
		// TODO: (#405) If triple buffering is needed, SwapInterval(0) should be called,
		// but is this correct? If glfw.SwapInterval(0) and the driver doesn't support triple
		// buffering, what will happen?
		if u.vsync {
			glfw.SwapInterval(1)
		} else {
			glfw.SwapInterval(0)
		}
	}
	u.Graphics().SetVsyncEnabled(u.vsync)
}

// currentMonitor returns the current active monitor.
//
// The given window might or might not be used to detect the monitor.
//
// currentMonitor must be called on the main thread.
func currentMonitor(window *glfw.Window) *glfw.Monitor {
	// GetMonitor is available only on fullscreen.
	if m := window.GetMonitor(); m != nil {
		return m
	}

	// Getting a monitor from a window position is not reliable in general (e.g., when a window is put across
	// multiple monitors, or, before SetWindowPosition is called.).
	// Get the monitor which the current window belongs to. This requires OS API.
	if m := currentMonitorByOS(window); m != nil {
		return m
	}

	// As the fallback, detect the monitor from the window.
	if m := getCachedMonitor(window.GetPos()); m != nil {
		return m.m
	}
	return glfw.GetPrimaryMonitor()
}

func (u *UserInterface) SetScreenTransparent(transparent bool) {
	if !u.isRunning() {
		u.setInitScreenTransparent(transparent)
		return
	}
	panic("glfw: SetScreenTransparent can't be called after the main loop starts")
}

func (u *UserInterface) IsScreenTransparent() bool {
	if !u.isRunning() {
		return u.isInitScreenTransparent()
	}
	val := false
	_ = u.t.Call(func() error {
		val = u.window.GetAttrib(glfw.TransparentFramebuffer) == glfw.True
		return nil
	})
	return val
}

func (u *UserInterface) ResetForFrame() {
	// The offscreens must be updated every frame (#490).
	var w, h float64
	var changed bool
	_ = u.t.Call(func() error {
		w, h, changed = u.updateSize()
		return nil
	})
	if changed {
		u.context.Layout(w, h)
	}
	u.input.resetForFrame()
}

func (u *UserInterface) MonitorPosition() (int, int) {
	if !u.isRunning() {
		return u.monitorPosition()
	}
	var mx, my int
	_ = u.t.Call(func() error {
		mx, my = u.monitorPosition()
		return nil
	})
	return mx, my
}

func (u *UserInterface) SetInitFocused(focused bool) {
	if u.isRunning() {
		panic("ui: SetInitFocused must be called before the main loop")
	}
	u.setInitFocused(focused)
}

func (u *UserInterface) monitorPosition() (int, int) {
	// TODO: fromGLFWMonitorPixel might be required.
	return currentMonitor(u.window).GetPos()
}

func (u *UserInterface) Input() driver.Input {
	return &u.input
}

func (u *UserInterface) Window() driver.Window {
	return &u.iwindow
}
