// NextTrace iOS shell (TrollStore build).
//
// Mirrors mobile/android: the NextTrace executable bundled in the app is
// started in deploy mode on a loopback port and its Web UI is shown in a
// WKWebView, so probes originate from this device.
//
// TrollStore lets the app run outside the sandbox, which is what allows it to
// spawn the bundled helper at all. The helper runs as the mobile user: iOS has
// no libpcap, so NextTrace only does ICMP there, and its datagram ICMP socket
// needs no privilege.

#import <UIKit/UIKit.h>
#import <WebKit/WebKit.h>

#import <arpa/inet.h>
#import <fcntl.h>
#import <netinet/in.h>
#import <signal.h>
#import <spawn.h>
#import <sys/socket.h>
#import <sys/wait.h>
#import <unistd.h>

extern char **environ;

static NSString *const kLoopback = @"127.0.0.1";
static const int kPreferredPort = 31780;
static const NSTimeInterval kStartTimeout = 20.0;
static const NSUInteger kMaxLogBytes = 16 * 1024;

#pragma mark - Helper process

@interface NTHelper : NSObject
@property (nonatomic, readonly) int port;
- (BOOL)isAlive;
- (BOOL)startWithError:(NSString **)error;
- (void)stop;
- (NSString *)recentLog;
@end

@implementation NTHelper {
    pid_t _pid;
    int _stdinWriteFD;
    NSMutableData *_log;
    NSLock *_logLock;
}

- (instancetype)init {
    if ((self = [super init])) {
        _pid = 0;
        _stdinWriteFD = -1;
        _log = [NSMutableData data];
        _logLock = [NSLock new];
    }
    return self;
}

- (void)appendLog:(NSData *)data {
    [_logLock lock];
    [_log appendData:data];
    if (_log.length > kMaxLogBytes) {
        [_log replaceBytesInRange:NSMakeRange(0, _log.length - kMaxLogBytes) withBytes:NULL length:0];
    }
    [_logLock unlock];
}

- (NSString *)recentLog {
    [_logLock lock];
    NSString *text = [[NSString alloc] initWithData:_log encoding:NSUTF8StringEncoding];
    if (text == nil) {
        text = [[NSString alloc] initWithData:_log encoding:NSISOLatin1StringEncoding];
    }
    [_logLock unlock];
    return text ?: @"";
}

- (BOOL)isAlive {
    if (_pid <= 0) {
        return NO;
    }
    int status = 0;
    pid_t result = waitpid(_pid, &status, WNOHANG);
    if (result == 0) {
        return YES;
    }
    _pid = 0;
    return NO;
}

- (void)stop {
    // Closing its stdin makes the helper exit on its own
    // (NEXTTRACE_EXIT_ON_STDIN_CLOSE); SIGTERM covers a helper stuck before that.
    if (_stdinWriteFD >= 0) {
        close(_stdinWriteFD);
        _stdinWriteFD = -1;
    }
    if (_pid > 0) {
        kill(_pid, SIGTERM);
    }
}

// The page keeps its settings and recent targets in localStorage, which is
// scoped to the origin including the port. Prefer one fixed port so they
// survive restarts, and only fall back to a random one if it is taken.
+ (int)pickFreePort {
    int port = [self bindLoopbackPort:kPreferredPort];
    return port != 0 ? port : [self bindLoopbackPort:0];
}

// Binds 127.0.0.1:requested (0 = any) and returns the bound port, or 0.
+ (int)bindLoopbackPort:(int)requested {
    int fd = socket(AF_INET, SOCK_STREAM, 0);
    if (fd < 0) {
        return 0;
    }
    int reuse = 1;
    setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, &reuse, sizeof(reuse));
    struct sockaddr_in addr = {0};
    addr.sin_len = sizeof(addr);
    addr.sin_family = AF_INET;
    addr.sin_port = htons((uint16_t)requested);
    addr.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    socklen_t len = sizeof(addr);
    int port = 0;
    if (bind(fd, (struct sockaddr *)&addr, sizeof(addr)) == 0 &&
        getsockname(fd, (struct sockaddr *)&addr, &len) == 0) {
        port = ntohs(addr.sin_port);
    }
    close(fd);
    return port;
}

+ (BOOL)canConnectToPort:(int)port {
    int fd = socket(AF_INET, SOCK_STREAM, 0);
    if (fd < 0) {
        return NO;
    }
    struct sockaddr_in addr = {0};
    addr.sin_len = sizeof(addr);
    addr.sin_family = AF_INET;
    addr.sin_port = htons((uint16_t)port);
    addr.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    BOOL ok = connect(fd, (struct sockaddr *)&addr, sizeof(addr)) == 0;
    close(fd);
    return ok;
}

+ (NSString *)dataDirectory {
    NSString *base = NSSearchPathForDirectoriesInDomains(NSApplicationSupportDirectory, NSUserDomainMask, YES).firstObject;
    NSString *dir = [base stringByAppendingPathComponent:@"NextTrace"];
    [[NSFileManager defaultManager] createDirectoryAtPath:dir withIntermediateDirectories:YES attributes:nil error:nil];
    return dir;
}

// Spawns the helper with stdin/stdout/stderr on pipes. Returns 0 or an errno.
- (int)spawn:(NSString *)binary port:(int)port {
    int inPipe[2], outPipe[2];
    if (pipe(inPipe) != 0) {
        return errno;
    }
    if (pipe(outPipe) != 0) {
        int err = errno;
        close(inPipe[0]);
        close(inPipe[1]);
        return err;
    }

    posix_spawn_file_actions_t actions;
    posix_spawn_file_actions_init(&actions);
    posix_spawn_file_actions_adddup2(&actions, inPipe[0], STDIN_FILENO);
    posix_spawn_file_actions_adddup2(&actions, outPipe[1], STDOUT_FILENO);
    posix_spawn_file_actions_adddup2(&actions, outPipe[1], STDERR_FILENO);

    posix_spawnattr_t attr;
    posix_spawnattr_init(&attr);
    // Close every other descriptor in the child. In particular it must not
    // inherit the write end of its own stdin pipe, or stdin would never hit EOF.
    posix_spawnattr_setflags(&attr, POSIX_SPAWN_CLOEXEC_DEFAULT);

    NSString *home = [NTHelper dataDirectory];
    NSString *listen = [NSString stringWithFormat:@"%@:%d", kLoopback, port];
    const char *argv[] = {binary.fileSystemRepresentation, "--deploy", "--listen", listen.UTF8String, NULL};

    NSMutableArray<NSString *> *envStrings = [NSMutableArray array];
    for (char **e = environ; e != NULL && *e != NULL; e++) {
        NSString *entry = [NSString stringWithUTF8String:*e];
        if (entry != nil && ![entry hasPrefix:@"HOME="] && ![entry hasPrefix:@"TMPDIR="]) {
            [envStrings addObject:entry];
        }
    }
    [envStrings addObject:[@"HOME=" stringByAppendingString:home]];
    [envStrings addObject:[@"XDG_CONFIG_HOME=" stringByAppendingString:[home stringByAppendingPathComponent:@".config"]]];
    [envStrings addObject:[@"TMPDIR=" stringByAppendingString:NSTemporaryDirectory()]];
    [envStrings addObject:@"NEXTTRACE_EXIT_ON_STDIN_CLOSE=1"];
    const char **envp = calloc(envStrings.count + 1, sizeof(char *));
    for (NSUInteger i = 0; i < envStrings.count; i++) {
        envp[i] = envStrings[i].UTF8String;
    }

    pid_t pid = 0;
    int rc = posix_spawn(&pid, binary.fileSystemRepresentation, &actions, &attr, (char *const *)argv, (char *const *)envp);
    free(envp);
    posix_spawn_file_actions_destroy(&actions);
    posix_spawnattr_destroy(&attr);
    close(inPipe[0]);
    close(outPipe[1]);

    if (rc != 0) {
        close(inPipe[1]);
        close(outPipe[0]);
        return rc;
    }

    _pid = pid;
    _stdinWriteFD = inPipe[1];
    fcntl(_stdinWriteFD, F_SETFD, FD_CLOEXEC);
    int readFD = outPipe[0];
    fcntl(readFD, F_SETFD, FD_CLOEXEC);
    __weak NTHelper *weakSelf = self;
    dispatch_async(dispatch_get_global_queue(QOS_CLASS_UTILITY, 0), ^{
        char buffer[4096];
        ssize_t n;
        while ((n = read(readFD, buffer, sizeof(buffer))) > 0) {
            [weakSelf appendLog:[NSData dataWithBytes:buffer length:(NSUInteger)n]];
        }
        close(readFD);
    });
    return 0;
}

// Waits until the helper accepts connections, or it dies, or the timeout hits.
- (BOOL)waitUntilReady:(int)port {
    NSDate *deadline = [NSDate dateWithTimeIntervalSinceNow:kStartTimeout];
    while ([deadline timeIntervalSinceNow] > 0) {
        if (![self isAlive]) {
            return NO;
        }
        if ([NTHelper canConnectToPort:port]) {
            return YES;
        }
        usleep(100 * 1000);
    }
    return NO;
}

- (BOOL)startWithError:(NSString **)error {
    if ([self isAlive]) {
        return YES;
    }
    NSString *binary = [[NSBundle mainBundle] pathForAuxiliaryExecutable:@"nexttrace-helper"];
    if (binary == nil || ![[NSFileManager defaultManager] isExecutableFileAtPath:binary]) {
        if (error) *error = @"nexttrace-helper is missing from the app bundle";
        return NO;
    }

    int port = [NTHelper pickFreePort];
    if (port == 0) {
        if (error) *error = @"could not find a free loopback port";
        return NO;
    }
    int rc = [self spawn:binary port:port];
    if (rc != 0) {
        if (error) *error = [NSString stringWithFormat:@"could not start the NextTrace helper: %s", strerror(rc)];
        return NO;
    }
    if (![self waitUntilReady:port]) {
        [self stop];
        if (error) *error = @"NextTrace helper did not become ready";
        return NO;
    }
    _port = port;
    return YES;
}

@end

#pragma mark - UI

@interface NTViewController : UIViewController <WKNavigationDelegate, WKUIDelegate>
- (void)ensureRunning;
- (void)shutdown;
@end

@implementation NTViewController {
    NTHelper *_helper;
    WKWebView *_webView;
    UITextView *_statusView;
    BOOL _starting;
    BOOL _loaded;
}

- (void)viewDidLoad {
    [super viewDidLoad];
    // Matches --shell-bg in server/web/assets/app_shell.css, so the status bar
    // area and the page read as one surface.
    UIColor *background = [UIColor colorWithRed:11 / 255.0 green:18 / 255.0 blue:32 / 255.0 alpha:1];
    self.view.backgroundColor = background;
    _helper = [NTHelper new];

    WKWebViewConfiguration *config = [WKWebViewConfiguration new];
    _webView = [[WKWebView alloc] initWithFrame:CGRectZero configuration:config];
    _webView.navigationDelegate = self;
    _webView.UIDelegate = self;
    // Behave like an app surface, not a browser page: no white flash, no
    // rubber-banding, no link previews.
    _webView.opaque = NO;
    _webView.backgroundColor = background;
    _webView.scrollView.backgroundColor = background;
    _webView.scrollView.bounces = NO;
    _webView.scrollView.showsHorizontalScrollIndicator = NO;
    _webView.scrollView.contentInsetAdjustmentBehavior = UIScrollViewContentInsetAdjustmentNever;
    _webView.allowsLinkPreview = NO;
    _webView.hidden = YES;
    _webView.translatesAutoresizingMaskIntoConstraints = NO;
    [self.view addSubview:_webView];

    _statusView = [UITextView new];
    _statusView.editable = NO;
    _statusView.backgroundColor = UIColor.clearColor;
    _statusView.textColor = UIColor.lightGrayColor;
    _statusView.font = [UIFont monospacedSystemFontOfSize:12 weight:UIFontWeightRegular];
    _statusView.translatesAutoresizingMaskIntoConstraints = NO;
    [_statusView addGestureRecognizer:[[UITapGestureRecognizer alloc] initWithTarget:self action:@selector(ensureRunning)]];
    [self.view addSubview:_statusView];

    UILayoutGuide *safe = self.view.safeAreaLayoutGuide;
    for (UIView *view in @[_webView, _statusView]) {
        [NSLayoutConstraint activateConstraints:@[
            [view.topAnchor constraintEqualToAnchor:safe.topAnchor],
            [view.bottomAnchor constraintEqualToAnchor:safe.bottomAnchor],
            [view.leadingAnchor constraintEqualToAnchor:safe.leadingAnchor],
            [view.trailingAnchor constraintEqualToAnchor:safe.trailingAnchor],
        ]];
    }
    [self ensureRunning];
}

- (UIStatusBarStyle)preferredStatusBarStyle {
    return UIStatusBarStyleLightContent;
}

- (void)showStatus:(NSString *)text {
    _statusView.text = text;
    _statusView.hidden = NO;
    _webView.hidden = YES;
}

// Starts the helper if it is not running and (re)loads the console.
- (void)ensureRunning {
    if (_starting) {
        return;
    }
    if (_loaded && [_helper isAlive]) {
        return;
    }
    _starting = YES;
    _loaded = NO;
    [self showStatus:@"Starting NextTrace…"];
    NTHelper *helper = _helper;
    dispatch_async(dispatch_get_global_queue(QOS_CLASS_USER_INITIATED, 0), ^{
        NSString *error = nil;
        BOOL ok = [helper startWithError:&error];
        dispatch_async(dispatch_get_main_queue(), ^{
            self->_starting = NO;
            if (!ok) {
                [self showStatus:[NSString stringWithFormat:@"%@\nTap to retry.\n\n%@",
                                  error ?: @"NextTrace failed to start", [helper recentLog]]];
                return;
            }
            // shell=app switches the console to its app layout (app_shell.js).
            NSString *url = [NSString stringWithFormat:@"http://%@:%d/?shell=app", kLoopback, helper.port];
            [self->_webView loadRequest:[NSURLRequest requestWithURL:[NSURL URLWithString:url]]];
        });
    });
}

- (void)shutdown {
    [_helper stop];
}

- (void)webView:(WKWebView *)webView didFinishNavigation:(WKNavigation *)navigation {
    _loaded = YES;
    _statusView.hidden = YES;
    _webView.hidden = NO;
}

- (void)failWithError:(NSError *)error {
    _loaded = NO;
    [self showStatus:[NSString stringWithFormat:@"%@\nTap to retry.\n\n%@",
                      error.localizedDescription, [_helper recentLog]]];
}

- (void)webView:(WKWebView *)webView didFailProvisionalNavigation:(WKNavigation *)navigation withError:(NSError *)error {
    [self failWithError:error];
}

- (void)webView:(WKWebView *)webView didFailNavigation:(WKNavigation *)navigation withError:(NSError *)error {
    [self failWithError:error];
}

- (void)webView:(WKWebView *)webView
    decidePolicyForNavigationAction:(WKNavigationAction *)action
                    decisionHandler:(void (^)(WKNavigationActionPolicy))decisionHandler {
    NSURL *url = action.request.URL;
    BOOL local = [url.host isEqualToString:kLoopback] || [url.scheme isEqualToString:@"about"] ||
                 [url.scheme isEqualToString:@"blob"] || [url.scheme isEqualToString:@"data"];
    BOOL subframe = action.targetFrame != nil && !action.targetFrame.isMainFrame;
    if (local || subframe) {
        decisionHandler(WKNavigationActionPolicyAllow);
        return;
    }
    // Anything that leaves the local console opens in the browser.
    [[UIApplication sharedApplication] openURL:url options:@{} completionHandler:nil];
    decisionHandler(WKNavigationActionPolicyCancel);
}

- (WKWebView *)webView:(WKWebView *)webView
    createWebViewWithConfiguration:(WKWebViewConfiguration *)configuration
               forNavigationAction:(WKNavigationAction *)action
                    windowFeatures:(WKWindowFeatures *)windowFeatures {
    // target=_blank links: hand them to the browser instead of opening a window.
    if (action.request.URL != nil) {
        [[UIApplication sharedApplication] openURL:action.request.URL options:@{} completionHandler:nil];
    }
    return nil;
}

- (void)webView:(WKWebView *)webView
    runJavaScriptAlertPanelWithMessage:(NSString *)message
                      initiatedByFrame:(WKFrameInfo *)frame
                     completionHandler:(void (^)(void))completionHandler {
    UIAlertController *alert = [UIAlertController alertControllerWithTitle:nil message:message preferredStyle:UIAlertControllerStyleAlert];
    [alert addAction:[UIAlertAction actionWithTitle:@"OK" style:UIAlertActionStyleDefault handler:^(UIAlertAction *a) { completionHandler(); }]];
    [self presentViewController:alert animated:YES completion:nil];
}

- (void)webView:(WKWebView *)webView
    runJavaScriptConfirmPanelWithMessage:(NSString *)message
                        initiatedByFrame:(WKFrameInfo *)frame
                       completionHandler:(void (^)(BOOL))completionHandler {
    UIAlertController *alert = [UIAlertController alertControllerWithTitle:nil message:message preferredStyle:UIAlertControllerStyleAlert];
    [alert addAction:[UIAlertAction actionWithTitle:@"Cancel" style:UIAlertActionStyleCancel handler:^(UIAlertAction *a) { completionHandler(NO); }]];
    [alert addAction:[UIAlertAction actionWithTitle:@"OK" style:UIAlertActionStyleDefault handler:^(UIAlertAction *a) { completionHandler(YES); }]];
    [self presentViewController:alert animated:YES completion:nil];
}

@end

#pragma mark - App

@interface NTAppDelegate : UIResponder <UIApplicationDelegate>
@property (nonatomic, strong) UIWindow *window;
@end

@implementation NTAppDelegate {
    NTViewController *_controller;
}

- (BOOL)application:(UIApplication *)application didFinishLaunchingWithOptions:(NSDictionary *)options {
    signal(SIGPIPE, SIG_IGN);
    _controller = [NTViewController new];
    self.window = [[UIWindow alloc] initWithFrame:UIScreen.mainScreen.bounds];
    self.window.rootViewController = _controller;
    [self.window makeKeyAndVisible];
    return YES;
}

- (void)applicationWillEnterForeground:(UIApplication *)application {
    // The helper may have been reclaimed while the app was suspended.
    [_controller ensureRunning];
}

- (void)applicationWillTerminate:(UIApplication *)application {
    [_controller shutdown];
}

@end

int main(int argc, char *argv[]) {
    @autoreleasepool {
        return UIApplicationMain(argc, argv, nil, NSStringFromClass([NTAppDelegate class]));
    }
}
