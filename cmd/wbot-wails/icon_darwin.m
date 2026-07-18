//go:build wails && darwin && cgo

#import <Cocoa/Cocoa.h>

void SetWbotApplicationIcon(const void *bytes, int length) {
    @autoreleasepool {
        NSData *data = [[NSData alloc] initWithBytes:bytes length:(NSUInteger)length];
        NSImage *image = [[NSImage alloc] initWithData:data];
        [data release];
        if (image == nil) {
            return;
        }
        [NSApp performSelectorOnMainThread:@selector(setApplicationIconImage:)
                               withObject:image
                            waitUntilDone:YES];
        [image release];
    }
}
