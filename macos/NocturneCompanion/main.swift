import AppKit
import UserNotifications
import Darwin

// No shell, worker credential, app-killing or machine-reboot access.
final class Control {
    let path: String
    let owner: uid_t
    init(path: String, owner: uid_t) { self.path = path; self.owner = owner }
    func request(_ payload: [String: Any]) throws -> [String: Any] {
        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else { throw NSError(domain: "socket", code: 1) }
        defer { close(fd) }
        var timeout = timeval(tv_sec: 2, tv_usec: 0)
        setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, socklen_t(MemoryLayout<timeval>.size))
        setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &timeout, socklen_t(MemoryLayout<timeval>.size))
        var noSignal: Int32 = 1
        setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &noSignal, socklen_t(MemoryLayout<Int32>.size))
        var address = sockaddr_un()
        address.sun_family = sa_family_t(AF_UNIX)
        let bytes = Array(path.utf8CString)
        guard bytes.count <= MemoryLayout.size(ofValue: address.sun_path) else { throw NSError(domain: "path", code: 1) }
        withUnsafeMutableBytes(of: &address.sun_path) { dest in bytes.withUnsafeBytes { dest.copyBytes(from: $0) } }
        let connected = withUnsafePointer(to: &address) { ptr in
            ptr.withMemoryRebound(to: sockaddr.self, capacity: 1) { connect(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) }
        }
        var uid: uid_t = 0; var gid: gid_t = 0
        guard connected == 0, getpeereid(fd, &uid, &gid) == 0, uid == owner else { throw NSError(domain: "authentication", code: 1) }
        var data = try JSONSerialization.data(withJSONObject: payload); data.append(10)
        try data.withUnsafeBytes { bytes in
            var offset = 0
            while offset < bytes.count {
                let n = write(fd, bytes.baseAddress!.advanced(by: offset), bytes.count - offset)
                guard n > 0 else { throw NSError(domain: "write", code: 1) }; offset += n
            }
        }
        var response = Data(); var buffer = [UInt8](repeating: 0, count: 512)
        while !response.contains(10) && response.count <= 4096 {
            let n = read(fd, &buffer, buffer.count)
            guard n > 0 else { throw NSError(domain: "read", code: 1) }
            response.append(contentsOf: buffer.prefix(n))
        }
        guard response.count <= 4096, let object = try JSONSerialization.jsonObject(with: response) as? [String: Any], object["ok"] as? Bool == true,
              let status = object["status"] as? [String: Any] else { throw NSError(domain: "protocol", code: 1) }
        return status
    }
}

final class Companion: NSObject, NSApplicationDelegate, UNUserNotificationCenterDelegate {
    let item = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
    let control: Control
    let queue = DispatchQueue(label: "io.nocturne.companion.control")
    var timer: Timer?
    var warning: String?
    var capable = false
    var pendingPause: Int?
    let stateItem = NSMenuItem(title: "Nocturne: connecting", action: nil, keyEquivalent: "")
    init(control: Control) { self.control = control }
    func applicationDidFinishLaunching(_ notification: Notification) {
        item.button?.title = "☾"
        item.button?.setAccessibilityLabel("Nocturne participation")
        let menu = NSMenu(); menu.addItem(stateItem); menu.addItem(.separator())
        for (label, action) in [("Pause 1 hour", #selector(pauseOne)), ("Pause 2 hours", #selector(pauseTwo)), ("Resume", #selector(resume))] {
            let entry = NSMenuItem(title: label, action: action, keyEquivalent: ""); entry.target = self; menu.addItem(entry)
        }
        menu.addItem(.separator())
        let quit = NSMenuItem(title: "Quit (holds processing)", action: #selector(quitApp), keyEquivalent: "q"); quit.target = self; menu.addItem(quit)
        item.menu = menu
        let center = UNUserNotificationCenter.current(); center.delegate = self
        center.setNotificationCategories([UNNotificationCategory(identifier: "NIGHT", actions: [
            UNNotificationAction(identifier: "ONE", title: "Pause 1 hour"),
            UNNotificationAction(identifier: "TWO", title: "Pause 2 hours")], intentIdentifiers: [])])
        center.requestAuthorization(options: [.alert, .sound]) { _, _ in self.tick() }
        timer = Timer.scheduledTimer(withTimeInterval: 3, repeats: true) { _ in self.tick() }
    }
    func tick() {
        UNUserNotificationCenter.current().getNotificationSettings { settings in
            let permitted = settings.authorizationStatus == .authorized && settings.alertSetting == .enabled
            self.queue.async {
                self.capable = permitted
                var body: [String: Any] = ["op": "ui_heartbeat", "notification_capable": permitted]
                if let id = self.warning { body["warning_id"] = id }
                if let hours = self.pendingPause { self.send(["op": "pause", "hours": hours]) }
                else { self.send(body) }
            }
        }
    }
    func send(_ body: [String: Any]) {
        do {
            let status = try control.request(body)
            if body["op"] as? String == "pause" { pendingPause = nil }
            let reason = status["reason"] as? String ?? "unavailable"
            let remaining = status["pause_remaining"] as? Int ?? 0
            DispatchQueue.main.async { self.stateItem.title = remaining > 0 ? "Paused: \((remaining + 59) / 60) min" : "Nocturne: \(reason)" }
            guard capable, let id = status["warning_id"] as? String else {
                warning = nil; UNUserNotificationCenter.current().removeAllDeliveredNotifications(); return
            }
            if warning != id {
                let content = UNMutableNotificationContent()
                content.title = "Nocturne would like to start"
                content.body = "Working late? Pause for 1 or 2 hours. If automatic participation is enabled, processing starts after the warning only while your Mac is idle and safe."
                content.categoryIdentifier = "NIGHT"
                UNUserNotificationCenter.current().removeAllDeliveredNotifications()
                UNUserNotificationCenter.current().add(UNNotificationRequest(identifier: id, content: content, trigger: nil)) { error in
                    self.queue.async {
                        // Submission is NOT proof of visibility under Focus or inactivity.
                        if error == nil { self.warning = id }
                        else { self.warning = nil; self.capable = false; _ = try? self.control.request(["op": "ui_heartbeat", "notification_capable": false]) }
                    }
                }
            }
        } catch {
            warning = nil
            DispatchQueue.main.async { self.stateItem.title = self.pendingPause != nil ? "Pause not confirmed: retrying" : "Nocturne: unavailable (state unknown)" }
        }
    }
    func action(_ body: [String: Any]) { queue.async { if body["op"] as? String == "pause" { self.pendingPause = body["hours"] as? Int }; self.warning = nil; self.send(body) } }
    @objc func pauseOne() { action(["op": "pause", "hours": 1]) }
    @objc func pauseTwo() { action(["op": "pause", "hours": 2]) }
    @objc func resume() { action(["op": "resume"]) }
    @objc func quitApp() { NSApp.terminate(nil) }
    func userNotificationCenter(_ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse, withCompletionHandler completionHandler: @escaping () -> Void) {
        if response.actionIdentifier == "ONE" { pauseOne() }
        if response.actionIdentifier == "TWO" { pauseTwo() }
        completionHandler()
    }
    func userNotificationCenter(_ center: UNUserNotificationCenter, willPresent notification: UNNotification, withCompletionHandler completionHandler: @escaping (UNNotificationPresentationOptions) -> Void) { completionHandler([.banner, .list]) }
}
let args = CommandLine.arguments
let path = args.count > 1 ? args[1] : "/private/var/db/nocturne-participation-ipc/control.sock"
let owner = args.count > 2 ? uid_t(args[2]) ?? 0 : 0
let app = NSApplication.shared
let delegate = Companion(control: Control(path: path, owner: owner))
app.delegate = delegate
app.setActivationPolicy(.accessory)
app.run()
