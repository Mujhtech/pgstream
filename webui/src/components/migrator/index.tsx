import React from "react";
import { z } from "zod";
import { useForm } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { Button } from "@/components/ui/button";
import {
  Form,
  FormControl,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from "@/components/ui/form";
import { Input } from "@/components/ui/input";
import { Separator } from "../ui/separator";
import LogSheet from "./log-sheet";

const formSchema = z.object({
  mysql: z.object({
    host: z.string().min(1),
    port: z.number().min(1),
    user: z.string().min(1),
    password: z.string().min(1),
    database: z.string().min(1),
  }),
  postgres: z.object({
    host: z.string().min(1),
    port: z.number().min(1),
    user: z.string().min(1),
    password: z.string().min(1),
    database: z.string().min(1),
    schema: z.string().optional(),
  }),
});

export default function Migrator() {
  const [logOpen, setLogOpen] = React.useState(false);

  const form = useForm<z.infer<typeof formSchema>>({
    resolver: zodResolver(formSchema),
    defaultValues: {
      mysql: {
        host: "",
        port: 3306,
        user: "",
        password: "",
        database: "",
      },
      postgres: {
        host: "",
        port: 5432,
        user: "",
        password: "",
        database: "",
        schema: "",
      },
    },
  });

  function onSubmit(values: z.infer<typeof formSchema>) {
    // Do something with the form values.
    // ✅ This will be type-safe and validated.
    console.log(values);
    setLogOpen(true);
  }

  return (
    <>
      <div className="grid grid-cols-1 md:grid-cols-[30px_1fr_30px] h-full justify-center px-10 max-w-screen mx-auto w-full">
        <div className="gap-4 col-span-1 hidden justify-between md:flex">
          <div className="border-x h-full"></div>
          <div className="border-x h-full"></div>
        </div>
        <div>
          <div className="flex flex-col h-full justify-center">
            <div className="flex flex-col items-center border rounded-2xl w-full max-w-4xl mx-auto">
              <Form {...form}>
                <form
                  onSubmit={form.handleSubmit(onSubmit)}
                  className="w-full flex flex-col gap-3"
                >
                  <div className="grid grid-cols-3 gap-4 p-3 pb-0">
                    <FormField
                      control={form.control}
                      name="mysql.host"
                      render={({ field }) => (
                        <FormItem className="col-span-3 md:col-span-2">
                          <FormLabel>MySQL Host</FormLabel>
                          <FormControl>
                            <Input placeholder="localhost" {...field} />
                          </FormControl>
                          <FormMessage />
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name="mysql.port"
                      render={({ field }) => (
                        <FormItem className="col-span-3 md:col-span-1">
                          <FormLabel>MySQL Port</FormLabel>
                          <FormControl>
                            <Input placeholder="3306" {...field} />
                          </FormControl>
                          <FormMessage />
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name="mysql.user"
                      render={({ field }) => (
                        <FormItem className="col-span-3 md:col-span-1">
                          <FormLabel>MySQL User</FormLabel>
                          <FormControl>
                            <Input placeholder="root" {...field} />
                          </FormControl>
                          <FormMessage />
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name="mysql.password"
                      render={({ field }) => (
                        <FormItem className="col-span-3 md:col-span-1">
                          <FormLabel>MySQL Password</FormLabel>
                          <FormControl>
                            <Input placeholder="" type="password" {...field} />
                          </FormControl>
                          <FormMessage />
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name="mysql.database"
                      render={({ field }) => (
                        <FormItem className="col-span-3 md:col-span-1">
                          <FormLabel>MySQL Database</FormLabel>
                          <FormControl>
                            <Input placeholder="mysql" {...field} />
                          </FormControl>
                          <FormMessage />
                        </FormItem>
                      )}
                    />
                  </div>
                  <Separator />
                  <div className="grid grid-cols-4 gap-4 p-3 pt-0">
                    <FormField
                      control={form.control}
                      name="postgres.host"
                      render={({ field }) => (
                        <FormItem className="col-span-4 md:col-span-3">
                          <FormLabel>PostgreSQL Host</FormLabel>
                          <FormControl>
                            <Input placeholder="localhost" {...field} />
                          </FormControl>
                          <FormMessage />
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name="postgres.port"
                      render={({ field }) => (
                        <FormItem className="col-span-4 md:col-span-1">
                          <FormLabel>PostgreSQL Port</FormLabel>
                          <FormControl>
                            <Input placeholder="3306" {...field} />
                          </FormControl>
                          <FormMessage />
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name="postgres.user"
                      render={({ field }) => (
                        <FormItem className="col-span-4 md:col-span-2">
                          <FormLabel>PostgreSQL User</FormLabel>
                          <FormControl>
                            <Input placeholder="postgres" {...field} />
                          </FormControl>
                          <FormMessage />
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name="postgres.password"
                      render={({ field }) => (
                        <FormItem className="col-span-4 md:col-span-2">
                          <FormLabel>PostgreSQL Password</FormLabel>
                          <FormControl>
                            <Input
                              placeholder="postgres"
                              type="password"
                              {...field}
                            />
                          </FormControl>
                          <FormMessage />
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name="postgres.database"
                      render={({ field }) => (
                        <FormItem className="col-span-4 md:col-span-2">
                          <FormLabel>PostgreSQL Database</FormLabel>
                          <FormControl>
                            <Input placeholder="pgstream" {...field} />
                          </FormControl>
                          <FormMessage />
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name="postgres.schema"
                      render={({ field }) => (
                        <FormItem className="col-span-4 md:col-span-2">
                          <FormLabel>PostgreSQL Schema</FormLabel>
                          <FormControl>
                            <Input placeholder="public" {...field} />
                          </FormControl>
                          <FormMessage />
                        </FormItem>
                      )}
                    />
                  </div>
                  <Separator />
                  <div className="px-3 pb-3 flex justify-end">
                    <Button type="submit">Submit</Button>
                  </div>
                </form>
              </Form>
            </div>
            <div className="mt-3 flex flex-col items-center justify-center">
              <p className="text-sm text-muted-foreground">
                This is a beta version of the migrator. It is not fully tested
                and may not work as expected.
              </p>
            </div>
          </div>
        </div>
        <div className="gap-4 col-span-1 hidden justify-between md:flex">
          <div className="border-x h-full"></div>
          <div className="border-x h-full"></div>
        </div>
      </div>
      <LogSheet open={logOpen} setOpen={setLogOpen} />
    </>
  );
}
