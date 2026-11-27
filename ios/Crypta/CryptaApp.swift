import SwiftUI

@main
struct CryptaApp: App {
    @StateObject private var model = AppModel()
    @Environment(\.scenePhase) private var phase
    @State private var background = UIBackgroundTaskIdentifier.invalid

    var body: some Scene {
        WindowGroup {
            RootView()
                .environmentObject(model)
                .environmentObject(model.store)
                .onAppear { model.start() }
                .onOpenURL { model.handle(url: $0) }
        }
        .onChange(of: phase) { phase in
            if phase == .active { model.resume() }
            // Ask iOS for a little extra time so short transfers can finish
            // after the app leaves the screen. Longer ones continue from
            // where they stopped the next time the app is opened.
            if phase == .background, model.activeTransfers > 0 {
                background = UIApplication.shared.beginBackgroundTask {
                    UIApplication.shared.endBackgroundTask(background)
                    background = .invalid
                }
            } else if phase == .active, background != .invalid {
                UIApplication.shared.endBackgroundTask(background)
                background = .invalid
            }
        }
    }
}

struct RootView: View {
    @EnvironmentObject var model: AppModel

    var body: some View {
        Group {
            if let error = model.startError {
                VStack(spacing: 12) {
                    Image(systemName: "exclamationmark.triangle").font(.largeTitle).foregroundStyle(.orange)
                    Text("无法启动").font(.headline)
                    Text(error).font(.footnote).foregroundStyle(.secondary).multilineTextAlignment(.center)
                }.padding()
            } else if !model.loaded {
                ProgressView()
            } else if !model.setup.configured {
                SetupView()
            } else {
                MainTabs()
            }
        }
        .sheet(isPresented: Binding(get: { model.pendingPair != nil && model.loaded }, set: { if !$0 { model.pendingPair = nil } })) {
            NavigationStack {
                PairView(server: model.pendingPair?.server ?? "", code: model.pendingPair?.code ?? "", autoStart: true)
            }
        }
    }
}

struct MainTabs: View {
    @EnvironmentObject var model: AppModel
    @StateObject private var nav = BrowserNav()

    var body: some View {
        TabView {
            NavigationStack(path: $nav.path) {
                BrowserView(path: "")
                    .navigationDestination(for: Item.self) { BrowserView(path: $0.path) }
            }
            .environmentObject(nav)
            .tabItem { Label("文件", systemImage: "folder") }

            NavigationStack { TransfersView() }
                .tabItem { Label("传输", systemImage: "arrow.up.arrow.down") }
                .badge(model.activeTransfers)

            NavigationStack { SettingsView() }
                .tabItem { Label("设置", systemImage: "gearshape") }
        }
    }
}
