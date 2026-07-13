import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";

export default function LogSheet({
  open,
  setOpen,
}: {
  open: boolean;
  setOpen: (open: boolean) => void;
}) {
  return (
    <Sheet open={open} onOpenChange={setOpen}>
      <SheetContent className="shadow-none">
        <SheetHeader>
          <SheetTitle>Web migrations are not enabled yet</SheetTitle>
          <SheetDescription>
            Your connection details were validated locally and were not sent or
            logged. Use the pgstream CLI to run the migration while the server
            API and live progress stream are being completed.
          </SheetDescription>
        </SheetHeader>
      </SheetContent>
    </Sheet>
  );
}
